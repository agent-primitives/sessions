package placement

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/internal/computestate"
	"github.com/agent-primitives/sessions/sqlitelog"
	"github.com/agent-primitives/sessions/wire"
)

// lockedBuffer is a log sink safe to read while a straggling goroutine still logs.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.buf.Bytes())
}

// guardLog is a sqlite session journal whose operations a test can fail or interleave with
// another writer. Hooks write through the embedded *sqlitelog.Log, so they never re-enter guardLog.
type guardLog struct {
	*sqlitelog.Log
	// onConfirm runs after the runtime returned compute, before the ledger confirm.
	onConfirm  func()
	confirmErr error
	// onFence runs inside NewFence after the fence is minted: a checkpoint or cancel in the window.
	onFence  func()
	fenceErr error
	readErr  error
	headErr  error
	// onAppend fails an append without writing it.
	onAppend func(api.Event) error
}

func (l *guardLog) ConfirmAllocation(ctx context.Context, attempt string, in api.Incarnation) error {
	if l.onConfirm != nil {
		l.onConfirm()
	}
	if l.confirmErr != nil {
		return l.confirmErr
	}
	return l.Log.ConfirmAllocation(ctx, attempt, in)
}

func (l *guardLog) NewFence() (int64, error) {
	if l.fenceErr != nil {
		return 0, l.fenceErr
	}
	fence, err := l.Log.NewFence()
	if l.onFence != nil {
		l.onFence()
	}
	return fence, err
}

func (l *guardLog) Read(from int64) ([]eventlog.Record, error) {
	if l.readErr != nil {
		return nil, l.readErr
	}
	return l.Log.Read(from)
}

func (l *guardLog) Head() (int64, error) {
	if l.headErr != nil {
		return 0, l.headErr
	}
	return l.Log.Head()
}

func (l *guardLog) Append(expected, fence int64, ev api.Event) (eventlog.Record, error) {
	if l.onAppend != nil {
		if err := l.onAppend(ev); err != nil {
			return eventlog.Record{}, err
		}
	}
	return l.Log.Append(expected, fence, ev)
}

// describeFailHarness answers the turn's own Describe with err.
type describeFailHarness struct {
	echoagent.Harness
	err error
}

func (h describeFailHarness) Describe(context.Context) (api.Descriptor, error) {
	return api.Descriptor{}, h.err
}

type guardOp string

const (
	guardExec   guardOp = "exec"
	guardResume guardOp = "resume"
)

type guardOutcome int

const (
	// guardSucceeds: no error and no release.
	guardSucceeds guardOutcome = iota
	// guardReleases: unowned compute, released through CleanupRuntime or reported.
	guardReleases
	// guardSkips: another owner may hold the compute; reported, never released.
	guardSkips
	// guardUnknown: the journal could not be read or verified, is shorter than before the
	// operation, or does not say which harness owns the compute; reported, never released.
	guardUnknown
	// guardUnchanged: the journal owns the compute; the error is the cause.
	guardUnchanged
	// guardRefused: Exec refused before allocating.
	guardRefused
)

// guardEnv is one operation under test. setup configures it before the Placer is built; hooks may
// use p, which is set by then.
type guardEnv struct {
	t        *testing.T
	ctx      context.Context
	cancel   context.CancelFunc
	store    *sqlitelog.Store
	path     string // the store's database file
	log      *guardLog
	backend  *allocationBackend
	opts     []Option
	p        *Placer
	uid      string
	expected *int64 // Exec's cursor (default: the seeded head); a checked Resume when set
}

// peerAppend commits ev as another writer would: under its own fence, at the current head.
func (e *guardEnv) peerAppend(ev api.Event) {
	e.t.Helper()
	fence, err := e.log.Log.NewFence()
	if err != nil {
		e.t.Fatal(err)
	}
	head, err := e.log.Log.Head()
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.log.Log.Append(head, fence, ev); err != nil {
		e.t.Fatal(err)
	}
}

// tamper rewrites the stored event at seq in place, keeping its prev_hash and hash, as storage
// corruption would.
func (e *guardEnv) tamper(seq int64, change func(*api.Event)) {
	e.t.Helper()
	db, err := sql.Open("sqlite", e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var blob []byte
	if err := db.QueryRow(`SELECT event FROM events WHERE session = ? AND seq = ?`, e.uid, seq).Scan(&blob); err != nil {
		e.t.Fatal(err)
	}
	pb := &v1.Event{}
	if err := proto.Unmarshal(blob, pb); err != nil {
		e.t.Fatal(err)
	}
	ev := wire.EventFromProto(pb)
	change(&ev)
	if blob, err = proto.Marshal(wire.EventToProto(ev)); err != nil {
		e.t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE events SET event = ? WHERE session = ? AND seq = ?`, blob, e.uid, seq); err != nil {
		e.t.Fatal(err)
	}
}

// truncate deletes the stored records from seq on, as a lost tail would: the rest still verifies.
func (e *guardEnv) truncate(seq int64) {
	e.t.Helper()
	db, err := sql.Open("sqlite", e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(`DELETE FROM events WHERE session = ? AND seq >= ?`, e.uid, seq); err != nil {
		e.t.Fatal(err)
	}
}

// parkOnApproval dials a harness whose turn requests a tool call that needs approval, so the
// controller parks it and the operation hands the compute to handoffPark. Closing the harness
// connection, the handoff's first step after it reads the journal, fails with closeErr.
func (e *guardEnv) parkOnApproval(closeErr error) {
	e.opts = append(e.opts, WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return approvalHarness{}, func() error { return closeErr }, nil
	}))
}

// approvalHarness is echoagent with a turn that stops on a tool call needing approval.
type approvalHarness struct{ echoagent.Harness }

func (approvalHarness) Run(ctx context.Context, _ *api.Start, sink api.EventSink) error {
	_, err := sink.ToolCall(ctx, api.ToolCall{ID: "c1", Tool: "write", IdempotencyKey: "key",
		Mediation: api.MediationRequiresApproval})
	return err
}

// closeParkedApproval parks a turn on approval through a Placer of its own, then closes the gate
// with controller.CloseApproval, approving it first when approved is set. The receipt is the host
// write APR-18 describes: NOT_EXECUTED, or OUTCOME_UNKNOWN after an approval, with no compute
// involved. handoffErr fails the park's handoff before its SUSPEND, as parkOnApproval's closeErr
// does, so the parked turn's compute stays LIVE.
func (e *guardEnv) closeParkedApproval(handoffErr error, approved bool) {
	e.t.Helper()
	var trace []string
	dial := func(api.Incarnation) (api.Harness, func() error, error) {
		return approvalHarness{}, func() error { return handoffErr }, nil
	}
	backend := snapshotBackend{&allocationBackend{trace: &trace}}
	parker := New(backend, echoagent.Model, WithDialer(dial))
	head, err := e.log.Log.Head()
	if err != nil {
		e.t.Fatal(err)
	}
	_, err = parker.Exec(e.ctx, e.log.Log, e.uid, nil, head)
	want := error(api.ErrApprovalParked)
	if handoffErr != nil {
		want = handoffErr
	}
	if !errors.Is(err, want) {
		e.t.Fatalf("parking Exec = %v, want %v", err, want)
	}
	recs, err := e.log.Log.Read(1)
	if err != nil {
		e.t.Fatal(err)
	}
	var ref api.ApprovalRef
	for _, r := range recs {
		if r.Event.Kind == api.EventApprovalRequest {
			ref = api.ApprovalRef{ExecutionID: r.Event.ExecutionID, ToolCallID: r.Event.Approval.ToolCallID,
				RequestSeq: r.Seq}
		}
	}
	last := recs[len(recs)-1].Event.Kind
	if suspended := last == api.EventLifecycle; suspended != (handoffErr == nil) {
		e.t.Fatalf("park left %v last, want SUSPEND only when the handoff succeeds", last)
	}
	if approved {
		decision := api.ApprovalDecision{ExecutionID: ref.ExecutionID, ToolCallID: ref.ToolCallID,
			RequestSeq: ref.RequestSeq, Approved: true}
		if _, err := controller.Approve(e.log.Log, decision); err != nil {
			e.t.Fatal(err)
		}
	}
	_, receipt, err := controller.CloseApproval(e.log.Log, ref, api.CallerIdentity{})
	if err != nil {
		e.t.Fatal(err)
	}
	code := api.ToolResultCodeNotExecuted
	if approved {
		code = api.ToolResultCodeOutcomeUnknown
	}
	if receipt.Event.Result.Code != code {
		e.t.Fatalf("receipt code = %v, want %v", receipt.Event.Result.Code, code)
	}
}

// snapshotBackend checkpoints any compute as a snapshot named by the session, which Restore
// allocates from.
type snapshotBackend struct{ *allocationBackend }

func (snapshotBackend) Snapshot(_ context.Context, inc api.Incarnation, _ api.SnapshotKind) (api.SnapshotRef, error) {
	return api.SnapshotRef{Local: inc.ID}, nil
}

func (e *guardEnv) dialErr(err error) {
	e.opts = append(e.opts, WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return nil, nil, err
	}))
}

var (
	errGuardInjected = errors.New("injected failure")
	errGuardRelease  = errors.New("injected release failure")
	suspendEvent     = api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{
		Kind: api.LifecycleSuspend, Snapshot: &api.SnapshotRef{Local: "s"}}}
)

// seedGuardJournal writes the session's journal before the operation, its turn recording harness
// and the compute instance it ran on:
//   - none: empty (NONE);
//   - live: a completed turn (LIVE);
//   - cold: a completed turn, then SUSPEND (COLD);
//   - pending: a started turn on harness, then SUSPEND (COLD with a turn for Resume to re-drive).
func seedGuardJournal(t *testing.T, log eventlog.Store, state, harness, instance string) {
	t.Helper()
	if state == "none" {
		return
	}
	desc, err := (echoagent.Harness{}).Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	events := []api.Event{
		{Kind: api.EventExecutionStart, ExecutionID: "x1", ExecutionStart: &api.ExecutionStart{
			InputCount: proto.Int64(1), Harness: harness, HarnessVersion: desc.Version, ComputeInstance: instance}},
		{Kind: api.EventInput, ExecutionID: "x1", Message: api.TextMessage("user", "hello")},
	}
	if state != "pending" {
		events = append(events, api.Event{Kind: api.EventEnd, ExecutionID: "x1", End: &api.HarnessEnd{State: "COMPLETED"}})
	}
	if state != "live" {
		events = append(events, suspendEvent)
	}
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	for i, ev := range events {
		if _, err := log.Append(int64(i), fence, ev); err != nil {
			t.Fatal(err)
		}
	}
}

// appendGuardEvents appends events at the head under a new fence.
func appendGuardEvents(t *testing.T, log eventlog.Store, events ...api.Event) {
	t.Helper()
	fence, err := log.NewFence()
	if err != nil {
		t.Fatal(err)
	}
	head, err := log.Head()
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if _, err := log.Append(head, fence, ev); err != nil {
			t.Fatal(err)
		}
		head++
	}
}

type guardRow struct {
	name string
	ops  []guardOp
	seed string // seedGuardJournal state; "" is cold
	// instance is the compute instance the seeded turn ran on.
	instance string
	want     guardOutcome
	cause    error // what the error must match (errors.Is); nil accepts any error
	// cleanup is what a report's Cleanup must match (errors.Is), when set.
	cleanup error
	// seedFn extends the seeded journal before the operation reads its head.
	seedFn func(e *guardEnv)
	setup  func(e *guardEnv)
}

// TestComputeGuard covers every return after a Resume or Exec allocated compute: compute nothing
// owns is released through CleanupRuntime exactly once or reported as ErrComputeLeftRunning;
// compute another owner may hold is reported, never released; compute the journal owns is left
// alone.
//
// controller.Resume writes nothing before checkHarness fails (row "harness mismatch") or the first
// harness call fails (row "lazy dial"). Its approval scan refuses a continuation past an open gate
// before continueAtGate runs, journaling nothing (controller's
// TestMemoryTierApprovalGateMustBeStreamTail), so that refusal takes the same path as a mismatch.
func TestComputeGuard(t *testing.T) {
	both := []guardOp{guardExec, guardResume}
	rows := []guardRow{
		{name: "success", ops: both, want: guardSucceeds},
		{name: "confirm failure", ops: both, want: guardReleases, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.log.confirmErr = errGuardInjected
		}},
		{name: "dial failure", ops: both, want: guardReleases, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.dialErr(errGuardInjected)
		}},
		{name: "checkpoint at registration", ops: both, want: guardSkips, cause: ErrCheckpointing, setup: func(e *guardEnv) {
			e.log.onConfirm = func() {
				if err := e.p.sessions.markCheckpoint(e.uid); err != nil {
					e.t.Fatal(err)
				}
			}
		}},
		{name: "recheck failure", ops: both, want: guardReleases, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.backend.live = true
			e.opts = append(e.opts, WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
				return describeFailHarness{err: errGuardInjected}, func() error { return nil }, nil
			}))
		}},
		{name: "fenced", ops: both, want: guardSkips, cause: eventlog.ErrFenced, setup: func(e *guardEnv) {
			e.log.fenceErr = fmt.Errorf("%w: injected", eventlog.ErrFenced)
		}},
		{name: "fence refused as deleted", ops: both, want: guardSkips, cause: eventlog.ErrDeleted, setup: func(e *guardEnv) {
			e.log.fenceErr = fmt.Errorf("%w: injected", eventlog.ErrDeleted)
		}},
		{name: "deleted during allocation", ops: both, want: guardSkips, cause: eventlog.ErrDeleted, setup: func(e *guardEnv) {
			e.log.onConfirm = func() {
				if _, err := e.store.DeleteSession(e.ctx, e.uid); err != nil {
					e.t.Fatal(err)
				}
			}
		}},
		{name: "checked resume head moved", ops: []guardOp{guardResume}, want: guardSkips, cause: eventlog.ErrConflict, setup: func(e *guardEnv) {
			head, err := e.log.Log.Head()
			if err != nil {
				e.t.Fatal(err)
			}
			e.expected = &head
			e.log.onConfirm = func() { e.peerAppend(suspendEvent) }
		}},
		// A checkpoint that superseded the turn may be snapshotting the compute or may fail before it
		// does; here it never records a SUSPEND, as a stateful Fork whose Snapshot fails does not.
		{name: "superseded by a checkpoint", ops: both, want: guardSkips, cause: errSuperseded, setup: func(e *guardEnv) {
			e.log.onFence = func() {
				if _, err := e.p.beginCheckpoint(e.uid); err != nil {
					e.t.Fatal(err)
				}
			}
		}},
		{name: "superseded by a checkpoint, live before allocation", ops: both, seed: "live", want: guardUnchanged, cause: errSuperseded, setup: func(e *guardEnv) {
			e.log.onFence = func() {
				if _, err := e.p.beginCheckpoint(e.uid); err != nil {
					e.t.Fatal(err)
				}
			}
		}},
		{name: "caller cancelled", ops: both, want: guardReleases, cause: context.Canceled, setup: func(e *guardEnv) {
			e.log.onFence = e.cancel
		}},
		{name: "invalid model policy at controller.New", ops: []guardOp{guardResume}, want: guardReleases, cause: api.ErrInvalidModelPolicy, setup: func(e *guardEnv) {
			e.opts = append(e.opts, WithModelPolicy(api.ModelPolicy{}))
		}},
		// Exec validates the model policy before allocating, so its controller.New fails after
		// allocation only for an unscoped tool executor.
		{name: "missing session uid at controller.New", ops: []guardOp{guardExec}, want: guardReleases, setup: func(e *guardEnv) {
			e.uid = ""
			e.opts = append(e.opts, WithToolExecutor(func(context.Context, controller.ToolCallContext, api.ToolCall) (api.ToolResult, error) {
				return api.ToolResult{}, nil
			}))
		}},
		// grpc.NewClient does not connect: an unreachable harness fails the controller's Describe.
		{name: "lazy dial", ops: both, seed: "pending", want: guardReleases, cause: ErrHarnessUnavailable, setup: func(e *guardEnv) {
			e.opts = nil // DefaultDial
			e.backend.address = "unix://" + filepath.Join(e.t.TempDir(), "missing.sock")
		}},
		{name: "harness mismatch", ops: []guardOp{guardResume}, seed: "pending-other", want: guardReleases},
		{name: "head read failure after the turn", ops: []guardOp{guardResume}, want: guardReleases, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.log.onConfirm = func() { e.log.headErr = errGuardInjected }
		}},
		{name: "resume marker append failure", ops: []guardOp{guardResume}, want: guardReleases, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.log.onAppend = func(ev api.Event) error {
				if ev.Kind == api.EventLifecycle && ev.Lifecycle.Kind == api.LifecycleResume {
					return errGuardInjected
				}
				return nil
			}
		}},
		{name: "stale cursor on a new session", ops: []guardOp{guardExec}, seed: "none", want: guardRefused, setup: func(e *guardEnv) {
			stale := int64(1)
			e.expected = &stale
		}},
		{name: "stale cursor on a cold session", ops: []guardOp{guardExec}, want: guardRefused, setup: func(e *guardEnv) {
			stale := int64(1)
			e.expected = &stale
		}},
		// A conflict that moved nothing is a caller's stale cursor, not a takeover.
		{name: "conflict without journal movement", ops: []guardOp{guardExec}, want: guardReleases, cause: eventlog.ErrConflict, setup: func(e *guardEnv) {
			e.log.onAppend = func(ev api.Event) error {
				if ev.Kind == api.EventExecutionStart {
					return fmt.Errorf("%w: injected", eventlog.ErrConflict)
				}
				return nil
			}
		}},
		{name: "peer append takes the cursor", ops: []guardOp{guardExec}, want: guardSkips, cause: eventlog.ErrConflict, setup: func(e *guardEnv) {
			e.log.onConfirm = func() { e.peerAppend(suspendEvent) }
		}},
		{name: "suspend during allocation", ops: both, want: guardSkips, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.log.onConfirm = func() { e.peerAppend(suspendEvent) }
			e.dialErr(errGuardInjected)
		}},
		// LIVE on this harness before the call does not make the journal the owner once a record
		// committed during the call left it COLD: the runtime may have woken compute the peer
		// suspended (substrate's attach resumes it), and the peer may resume it again.
		{name: "suspend during allocation, live before", ops: both, seed: "live", want: guardSkips,
			cause: errGuardInjected, setup: func(e *guardEnv) {
				e.log.onConfirm = func() { e.peerAppend(suspendEvent) }
				e.dialErr(errGuardInjected)
			}},
		{name: "approval during allocation", ops: both, want: guardSkips, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.log.onConfirm = func() {
				e.peerAppend(api.Event{Kind: api.EventApprovalRequest, ExecutionID: "x1"})
			}
			e.dialErr(errGuardInjected)
		}},
		{name: "live before allocation", ops: both, seed: "live", want: guardUnchanged, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.dialErr(errGuardInjected)
		}},
		{name: "turn journaled then failed", ops: both, seed: "pending", want: guardUnchanged, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.log.onAppend = func(ev api.Event) error {
				if ev.Kind == api.EventEnd {
					return errGuardInjected
				}
				return nil
			}
		}},
		{name: "journal unreadable", ops: both, want: guardUnknown, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.log.onConfirm = func() { e.log.readErr = errors.New("injected read failure") }
			e.dialErr(errGuardInjected)
		}},
		// The chain verifies from genesis only: without the lost SUSPEND the completed turn reads
		// LIVE on this harness, owning compute the journal had recorded COLD.
		{name: "journal tail lost during allocation", ops: both, want: guardUnknown, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.log.onConfirm = func() { e.truncate(4) }
			e.dialErr(errGuardInjected)
		}},
		{name: "journal emptied during allocation", ops: both, want: guardUnknown, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.log.onConfirm = func() { e.truncate(1) }
			e.dialErr(errGuardInjected)
		}},
		// The turn parks on approval, and handoffPark fails before its SUSPEND commits: the guard
		// stays armed, so a journal that cannot be read is reported rather than the bare cause.
		{name: "approval handoff, journal unreadable", ops: []guardOp{guardExec}, want: guardUnknown, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.parkOnApproval(nil)
			e.log.onAppend = func(ev api.Event) error {
				if ev.Kind == api.EventApprovalRequest {
					e.log.readErr = errGuardInjected
				}
				return nil
			}
		}},
		{name: "approval handoff, journal unreadable", ops: []guardOp{guardResume}, seed: "pending", want: guardUnknown, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.parkOnApproval(nil)
			e.log.onAppend = func(ev api.Event) error {
				if ev.Kind == api.EventApprovalRequest {
					e.log.readErr = errGuardInjected
				}
				return nil
			}
		}},
		// A handoff that fails on a readable journal leaves the parked turn's compute LIVE on this
		// harness, so the journal owns it.
		{name: "approval handoff, close failure", ops: []guardOp{guardExec}, want: guardUnchanged, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.parkOnApproval(errGuardInjected)
		}},
		{name: "approval handoff, close failure", ops: []guardOp{guardResume}, seed: "pending", want: guardUnchanged, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.parkOnApproval(errGuardInjected)
		}},
		// CloseApproval's receipt is a host write that proves no compute ran (APR-18): after the
		// park's SUSPEND the journal owns nothing, so compute a later operation allocated is
		// released or reported, not taken for the receipt's.
		{name: "closed approval, not executed, dial failure", ops: both, want: guardReleases, cause: errGuardInjected,
			seedFn: func(e *guardEnv) { e.closeParkedApproval(nil, false) },
			setup:  func(e *guardEnv) { e.dialErr(errGuardInjected) }},
		{name: "closed approval, outcome unknown, dial failure", ops: both, want: guardReleases, cause: errGuardInjected,
			seedFn: func(e *guardEnv) { e.closeParkedApproval(nil, true) },
			setup:  func(e *guardEnv) { e.dialErr(errGuardInjected) }},
		{name: "closed approval, not executed, confirm failure", ops: both, want: guardReleases, cause: errGuardInjected,
			seedFn: func(e *guardEnv) { e.closeParkedApproval(nil, false) },
			setup:  func(e *guardEnv) { e.log.confirmErr = errGuardInjected }},
		{name: "closed approval, outcome unknown, confirm failure", ops: both, want: guardReleases,
			cause:  errGuardInjected,
			seedFn: func(e *guardEnv) { e.closeParkedApproval(nil, true) },
			setup:  func(e *guardEnv) { e.log.confirmErr = errGuardInjected }},
		// Without the SUSPEND, the parked turn still owns its LIVE compute; the receipt changes
		// nothing.
		{name: "closed approval after a failed handoff, dial failure", ops: []guardOp{guardExec}, want: guardUnchanged, cause: errGuardInjected,
			seedFn: func(e *guardEnv) { e.closeParkedApproval(errGuardInjected, false) },
			setup:  func(e *guardEnv) { e.dialErr(errGuardInjected) }},
		// A SUSPEND corrupted into a RESUME reads LIVE on this harness; the chain fails to verify.
		{name: "tampered suspend reads live", ops: both, want: guardUnknown, cause: controller.ErrJournalTampered, cleanup: eventlog.ErrTampered, setup: func(e *guardEnv) {
			e.tamper(4, func(ev *api.Event) {
				if ev.Kind != api.EventLifecycle || ev.Lifecycle.Kind != api.LifecycleSuspend {
					e.t.Fatalf("seq 4 is %v, want SUSPEND", ev.Kind)
				}
				ev.Lifecycle.Kind = api.LifecycleResume
			})
		}},
		{name: "tampered turn on a cold journal", ops: both, want: guardUnknown, cause: controller.ErrJournalTampered, cleanup: eventlog.ErrTampered, setup: func(e *guardEnv) {
			e.tamper(2, func(ev *api.Event) { ev.Message = api.TextMessage("user", "tampered") })
		}},
		// The runtime replaced the instance the session's turn ran on: the controller refuses it
		// before appending, and the journal's LIVE is the replaced instance, not this one.
		{name: "replacement instance", ops: both, seed: "live", instance: "c1", want: guardReleases, cause: api.ErrMemoryStateLost, setup: func(e *guardEnv) {
			e.backend.computeInstance = "c2"
		}},
		{name: "replacement instance, dial failure", ops: both, seed: "live", instance: "c1", want: guardReleases, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.backend.computeInstance = "c2"
			e.dialErr(errGuardInjected)
		}},
		{name: "same instance, dial failure", ops: both, seed: "live", instance: "c1", want: guardUnchanged, cause: errGuardInjected, setup: func(e *guardEnv) {
			e.backend.computeInstance = "c1"
			e.dialErr(errGuardInjected)
		}},
		// A journal the controller rejects (a second EXECUTION_START of the seeded turn naming
		// another harness) does not say which harness left the compute LIVE, even on a standalone
		// Placer.
		{name: "conflicting EXECUTION_START, dial failure", ops: both, seed: "live", want: guardUnknown, cause: errGuardInjected, seedFn: func(e *guardEnv) {
			appendGuardEvents(e.t, e.log.Log, turnStart("x1", "other"))
		}, setup: func(e *guardEnv) {
			e.dialErr(errGuardInjected)
		}},
	}
	variants := []string{"release", "unsupported", "release-error"}
	for _, row := range rows {
		for _, op := range row.ops {
			for _, variant := range variants {
				t.Run(row.name+"/"+string(op)+"/"+variant, func(t *testing.T) {
					t.Parallel()
					runGuardRow(t, row, op, variant)
				})
			}
		}
	}
}

func runGuardRow(t *testing.T, row guardRow, op guardOp, variant string) {
	path := filepath.Join(t.TempDir(), "journal.db")
	store, err := sqlitelog.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var trace []string
	e := &guardEnv{t: t, ctx: ctx, cancel: cancel, store: store, path: path, log: &guardLog{Log: store.Session("s")},
		backend: &allocationBackend{trace: &trace}, uid: "s"}
	seed, harness := row.seed, "echo"
	switch seed {
	case "":
		seed = "cold"
	case "pending-other":
		seed, harness = "pending", "other"
	}
	seedGuardJournal(t, e.log.Log, seed, harness, row.instance)
	if row.seedFn != nil {
		row.seedFn(e)
	}
	before, err := e.log.Log.Read(1)
	if err != nil {
		t.Fatal(err)
	}
	e.opts = []Option{WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return echoagent.Harness{}, func() error { return nil }, nil
	})}
	if row.setup != nil {
		row.setup(e)
	}
	var backend Backend = e.backend
	var safe *safeAllocationBackend
	switch variant {
	case "release":
		safe = &safeAllocationBackend{allocationBackend: e.backend, key: "release"}
	case "release-error":
		safe = &safeAllocationBackend{allocationBackend: e.backend, key: "release", releaseErr: errGuardRelease}
	}
	if safe != nil {
		backend = safe
	}
	var logs lockedBuffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	e.p = New(backend, echoagent.Model, append(e.opts, WithLogger(logger))...)
	switch op {
	case guardExec:
		expected := int64(len(before))
		if e.expected != nil {
			expected = *e.expected
		}
		_, err = e.p.Exec(ctx, e.log, e.uid, nil, expected)
	case guardResume:
		var opts []ResumeOption
		if e.expected != nil {
			opts = append(opts, func(c *resumeConfig) { c.expectedLastSeq = e.expected })
		}
		err = e.p.Resume(ctx, e.log, e.uid, opts...)
	}

	var released []string
	if safe != nil {
		released = safe.released
	}
	if e.backend.stops != 0 {
		t.Errorf("generic Stop called %d times", e.backend.stops)
	}
	var left *ComputeLeftRunningError
	typed := errors.As(err, &left)
	if typed != errors.Is(err, ErrComputeLeftRunning) {
		t.Errorf("errors.As=%v but errors.Is(ErrComputeLeftRunning)=%v", typed, errors.Is(err, ErrComputeLeftRunning))
	}
	if row.want != guardSucceeds && row.want != guardRefused && row.cause != nil && !errors.Is(err, row.cause) {
		t.Errorf("error = %v, want cause %v", err, row.cause)
	}
	if typed {
		if left.Instance != "resource-"+e.uid {
			t.Errorf("Instance = %q, want the runtime's incarnation", left.Instance)
		}
		if !strings.HasPrefix(err.Error(), left.Cause.Error()+"; ") || !strings.Contains(err.Error(), fmt.Sprintf("%q", left.Instance)) {
			t.Errorf("message %q does not lead with the cause and name the instance", err)
		}
		if left.Cleanup == nil || !strings.Contains(err.Error(), left.Cleanup.Error()) {
			t.Errorf("message %q does not carry the cleanup outcome %v", err, left.Cleanup)
		}
		data := logs.Bytes()
		var warned []map[string]any
		for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			var record map[string]any
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatalf("decode log %q: %v", line, err)
			}
			if record["operation"] == "settle_compute" {
				warned = append(warned, record)
			}
		}
		if len(warned) != 1 || warned[0]["level"] != "WARN" || warned[0]["incarnation_id"] != left.Instance ||
			warned[0]["compute_instance"] != left.ComputeInstance || warned[0]["cleanup"] != left.Cleanup.Error() {
			t.Errorf("report logs = %v, want one WARN naming the instance, its compute instance and the cleanup outcome", warned)
		}
		for _, raw := range []string{errGuardRelease.Error(), "injected read failure"} {
			if bytes.Contains(data, []byte(raw)) {
				t.Errorf("logs carry the error text %q", raw)
			}
		}
	} else if bytes.Contains(logs.Bytes(), []byte("settle_compute")) {
		t.Errorf("logged a report for error %v", err)
	}
	wantReport := func(cleanup string) {
		t.Helper()
		if !typed || !strings.Contains(left.Cleanup.Error(), cleanup) {
			t.Fatalf("error = %v, want ComputeLeftRunningError with cleanup %q", err, cleanup)
		}
		if len(released) != 0 {
			t.Fatalf("released %v, want no release", released)
		}
	}
	switch row.want {
	case guardSucceeds:
		if err != nil {
			t.Fatal(err)
		}
		if len(released) != 0 {
			t.Fatalf("success released %v", released)
		}
		if want := []string{map[guardOp]string{guardExec: "create", guardResume: "restore"}[op]}; !reflect.DeepEqual(trace, want) {
			t.Fatalf("allocations = %v, want %v", trace, want)
		}
	case guardRefused:
		if !errors.Is(err, eventlog.ErrConflict) || typed {
			t.Fatalf("error = %v, want an untyped ErrConflict", err)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("expected_last_seq %d, head %d", *e.expected, len(before))) {
			t.Fatalf("error = %v, want the cursor and head", err)
		}
		if len(trace) != 0 || len(released) != 0 {
			t.Fatalf("refused Exec allocated %v, released %v", trace, released)
		}
		after, rerr := e.log.Log.Read(1)
		if rerr != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("refused Exec changed the journal (%v)", rerr)
		}
	case guardUnchanged:
		if err == nil || typed || len(released) != 0 {
			t.Fatalf("error = %v released = %v, want the cause alone and no release", err, released)
		}
	case guardSkips:
		wantReport("release skipped")
	case guardUnknown:
		wantReport("ownership unknown")
		if strings.Contains(err.Error(), "injected read failure") {
			t.Fatalf("message %q carries the journal's read error, want fixed text", err)
		}
		if row.cleanup != nil && !errors.Is(left.Cleanup, row.cleanup) {
			t.Fatalf("Cleanup = %v, want it to wrap %v", left.Cleanup, row.cleanup)
		}
		if row.cleanup != nil && strings.Contains(left.Cleanup.Error(), "seq") {
			t.Fatalf("Cleanup %q carries the verification error, want fixed text", left.Cleanup)
		}
	case guardReleases:
		if err == nil {
			t.Fatal("operation succeeded")
		}
		switch variant {
		case "release":
			if typed {
				t.Fatalf("error = %v, want the cause after a successful release", err)
			}
			if !reflect.DeepEqual(released, []string{e.uid}) {
				t.Fatalf("released %v, want exactly [%q]", released, e.uid)
			}
			if ctxErr := safe.releaseCtxErrs[0]; ctxErr != nil {
				t.Fatalf("release ran on a done context: %v", ctxErr)
			}
		case "unsupported":
			wantReport("does not implement CleanupRuntime")
		case "release-error":
			if !typed || !errors.Is(left.Cleanup, errGuardRelease) {
				t.Fatalf("error = %v, want ComputeLeftRunningError whose Cleanup wraps the release error", err)
			}
			if errors.Is(err, errGuardRelease) {
				t.Fatal("the release error joined the cause's errors.Is chain")
			}
			if strings.Contains(err.Error(), errGuardRelease.Error()) {
				t.Fatalf("message %q carries the backend's release error, want fixed text", err)
			}
			if !reflect.DeepEqual(released, []string{e.uid}) {
				t.Fatalf("released %v, want exactly [%q]", released, e.uid)
			}
		}
	}
}

// TestComputeGuardAcrossPlacers covers a Registry, where an ExecRequest.harness override runs a
// turn on a Placer other than the session's own: LIVE counts as the journal owning the compute
// only when what left it LIVE ran on the Placer whose backend returned it. A completed turn on A
// says nothing about the compute B's Create just returned, so a failed override onto B releases or
// reports it. A RESUME ran on the session's own harness or on the harness of any turn that was
// pending in the journal (every turn after the latest FORK marker), so when those name this Placer
// and another one, ownership is unknown; when none is this Placer, it releases.
func TestComputeGuardAcrossPlacers(t *testing.T) {
	resume := api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleResume}}
	fork := api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleFork}}
	end := api.Event{Kind: api.EventEnd, ExecutionID: "x1", End: &api.HarnessEnd{State: "COMPLETED"}}
	aStart := api.Event{Kind: api.EventExecutionStart, ExecutionID: "x2",
		ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0), Harness: "a"}}
	aEnd := api.Event{Kind: api.EventEnd, ExecutionID: "x2", End: &api.HarnessEnd{State: "COMPLETED"}}
	// Interleaved turns: x1 on C starts, x2 on B starts, then x1's records follow x2's START.
	c1, b2 := turnStart("x1", "c"), turnStart("x2", "b")
	e1, e2 := turnEnd("x1"), turnEnd("x2")
	type acrossRow struct {
		name string
		// seed is the seedGuardJournal state ("" is live); tail is appended after it.
		seed string
		tail []api.Event
		// recorded is the harness name the seeded turn recorded ("" is a legacy turn on the
		// session's own harness, a).
		recorded string
		on       string // the harness the Exec runs on; the session's own is a
		want     guardOutcome
		// peerTurn commits a turn on a during the allocation.
		peerTurn bool
		// failEnd lets the turn run and fails its END append instead of the dial.
		failEnd bool
		// failConfirm fails the allocation confirm instead of the dial.
		failConfirm bool
	}
	rows := []acrossRow{
		{name: "A's turn, override onto B", recorded: "a", on: "b", want: guardReleases},
		{name: "legacy turn, override onto B", recorded: "", on: "b", want: guardReleases},
		{name: "turn on a harness this host does not serve, override onto B", recorded: "gone", on: "b", want: guardReleases},
		{name: "turn on a harness this host does not serve, on A", recorded: "gone", on: "a", want: guardReleases},
		{name: "B's earlier override turn, override onto B", recorded: "b", on: "b", want: guardUnchanged},
		{name: "A's turn on A", recorded: "a", on: "a", want: guardUnchanged},
		{name: "alias of A's turn on A", recorded: "a-alias", on: "a", want: guardUnchanged},
		{name: "legacy turn on A", recorded: "", on: "a", want: guardUnchanged},
		{name: "peer turn on A during an override onto B", recorded: "a", on: "b", peerTurn: true, want: guardSkips},
		{name: "override turn journaled on B then failed", recorded: "a", on: "b", failEnd: true, want: guardUnchanged},
		// A fork child of a parent whose last turn ran on B, resumed with no pending turn: the
		// resume ran on A.
		{name: "fork child of B's turn resumed, override onto B", recorded: "b", tail: []api.Event{fork, resume}, on: "b", want: guardReleases},
		{name: "fork child of B's turn resumed, on A", recorded: "b", tail: []api.Event{fork, resume}, on: "a", want: guardUnchanged},
		{name: "fork child of B's turn resumed twice, override onto B", recorded: "b", tail: []api.Event{fork, resume, resume}, on: "b", want: guardReleases},
		{name: "fork child of B's turn resumed twice, on A", recorded: "b", tail: []api.Event{fork, resume, resume}, on: "a", want: guardUnchanged},
		// A resume whose pick of B for the pending turn went stale may append its RESUME after
		// any later record: a SUSPEND, another RESUME, or a later turn on A.
		{name: "B's turn, suspended, resumed, override onto B", recorded: "b", tail: []api.Event{suspendEvent, resume}, on: "b", want: guardUnknown},
		{name: "B's turn, suspended, resumed, on A", recorded: "b", tail: []api.Event{suspendEvent, resume}, on: "a", want: guardUnknown},
		{name: "B's turn re-driven, resumed twice, override onto B", seed: "pending", recorded: "b", tail: []api.Event{end, resume, resume}, on: "b", want: guardUnknown},
		{name: "B's turn re-driven, resumed twice, on A", seed: "pending", recorded: "b", tail: []api.Event{end, resume, resume}, on: "a", want: guardUnknown},
		{name: "B's turn re-driven, resumed twice, override onto C", seed: "pending", recorded: "b", tail: []api.Event{end, resume, resume}, on: "c", want: guardReleases},
		{name: "resume after B's turn, A's turn, resumed, override onto B", recorded: "b", tail: []api.Event{resume, aStart, aEnd, resume}, on: "b", want: guardUnknown},
		{name: "resume after B's turn, A's turn, resumed, on A", recorded: "b", tail: []api.Event{resume, aStart, aEnd, resume}, on: "a", want: guardUnknown},
		{name: "A's turn, resumed twice, on A", recorded: "a", tail: []api.Event{resume, resume}, on: "a", want: guardUnchanged},
		{name: "A's turn, resumed twice, override onto B", recorded: "a", tail: []api.Event{resume, resume}, on: "b", want: guardReleases},
		{name: "resume right after B's turn, on A", recorded: "b", tail: []api.Event{resume}, on: "a", want: guardUnknown},
		{name: "resume right after B's turn, override onto B", recorded: "b", tail: []api.Event{resume}, on: "b", want: guardUnknown},
		{name: "resume right after B's turn, override onto C", recorded: "b", tail: []api.Event{resume}, on: "c", want: guardReleases},
		{name: "resume right after A's turn, on A", recorded: "a", tail: []api.Event{resume}, on: "a", want: guardUnchanged},
		// A fork child re-drove the turn it inherited mid-turn on B across the FORK marker.
		{name: "fork child re-drove B's turn, then resumed, override onto B", seed: "pending", recorded: "b", tail: []api.Event{fork, end, resume}, on: "b", want: guardUnknown},
		{name: "fork child re-drove B's turn, then resumed, on A", seed: "pending", recorded: "b", tail: []api.Event{fork, end, resume}, on: "a", want: guardUnknown},
		{name: "fork child re-drove B's turn, override onto B", seed: "pending", recorded: "b", tail: []api.Event{fork, end}, on: "b", want: guardUnchanged},
		{name: "fork child re-drove B's turn, on A", seed: "pending", recorded: "b", tail: []api.Event{fork, end}, on: "a", want: guardReleases},
		// Interleaved executions: a record belongs to the turn its ExecutionID names, not to the
		// latest EXECUTION_START before it.
		{name: "C's turn ends after B's, override onto B", seed: "none", tail: []api.Event{c1, b2, e2, e1}, on: "b", want: guardReleases},
		{name: "C's turn ends after B's, override onto C", seed: "none", tail: []api.Event{c1, b2, e2, e1}, on: "c", want: guardUnchanged},
		{name: "C's turn ends after B's and a peer SUSPEND, override onto B", seed: "none", tail: []api.Event{c1, b2, e2, suspendEvent, e1}, on: "b", want: guardReleases},
		{name: "C's turn re-driven across FORK after B's START, override onto B", seed: "none", tail: []api.Event{c1, b2, fork, e1}, on: "b", want: guardReleases},
		{name: "C's turn re-driven across FORK after B's START, override onto C", seed: "none", tail: []api.Event{c1, b2, fork, e1}, on: "c", want: guardUnchanged},
		{name: "C's turn re-driven across FORK after B's START, resumed, override onto C", seed: "none", tail: []api.Event{c1, b2, fork, e1, resume}, on: "c", want: guardUnknown},
		{name: "C's turn re-driven across FORK after B's START, resumed, override onto B", seed: "none", tail: []api.Event{c1, b2, fork, e1, resume}, on: "b", want: guardReleases},
		{name: "C's turn ends after B's, resumed, override onto C", seed: "none", tail: []api.Event{c1, b2, e2, e1, resume}, on: "c", want: guardUnknown},
		{name: "C's turn ends after B's, resumed, override onto B", seed: "none", tail: []api.Event{c1, b2, e2, e1, resume}, on: "b", want: guardUnknown},
		{name: "A's turn re-driven across FORK after B's START, resumed, on A", seed: "none", tail: []api.Event{turnStart("x1", "a"), b2, fork, e1, resume}, on: "a", want: guardUnchanged},
		{name: "A's turn re-driven across FORK after B's START, resumed, override onto B", seed: "none", tail: []api.Event{turnStart("x1", "a"), b2, fork, e1, resume}, on: "b", want: guardReleases},
	}
	// Journals the controller rejects but the hash chain does not catch, with controls the
	// controller accepts. Each run fails at the dial and, separately, at the allocation confirm.
	b1 := turnStart("x1", "b")
	noPayload := func(id string) api.Event { return api.Event{Kind: api.EventExecutionStart, ExecutionID: id} }
	legacyInput := api.Event{Kind: api.EventInput, Message: api.TextMessage("user", "hi")}
	legacyEnd := api.Event{Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}}
	legacyOutput := api.Event{Kind: api.EventOutput, Message: api.TextMessage("assistant", "hi")}
	legacyError := api.Event{Kind: api.EventError, Err: &api.Error{Description: "failed"}}
	input1 := api.Event{Kind: api.EventInput, ExecutionID: "x1", Message: api.TextMessage("user", "hi")}
	toolCall1 := api.Event{Kind: api.EventToolCall, ExecutionID: "x1", ToolCall: &api.ToolCall{ID: "c1", Tool: "write"}}
	for _, row := range []acrossRow{
		// A repeated START naming the same harness does not change who ran the turn.
		{name: "repeated EXECUTION_START naming B, override onto B", seed: "none", tail: []api.Event{b1, b1}, on: "b", want: guardUnchanged},
		// Controls the controller accepts: a finished legacy turn (END or ERROR) before an older
		// writer's turn, which ran on the session's own harness.
		{name: "older writer's INPUT after a finished legacy turn, on A", seed: "none", tail: []api.Event{legacyInput, legacyEnd, input1}, on: "a", want: guardUnchanged},
		{name: "older writer's TOOL_CALL after a failed legacy turn, override onto B", seed: "none", tail: []api.Event{legacyInput, legacyError, toolCall1}, on: "b", want: guardReleases},
		// Conflicting STARTs, an ID-less or payload-less START, a late START or an ID-less record
		// after a modern one leave the journal unable to say which harness ran a turn: ownership
		// is unknown wherever the malformed record is, never kept or released on a guess.
		{name: "conflicting EXECUTION_STARTs for C then B, override onto B", seed: "none", tail: []api.Event{c1, b1}, on: "b", want: guardUnknown},
		{name: "conflicting EXECUTION_STARTs for C then B, override onto C", seed: "none", tail: []api.Event{c1, b1}, on: "c", want: guardUnknown},
		{name: "conflicting EXECUTION_STARTs for C then B, on A", seed: "none", tail: []api.Event{c1, b1}, on: "a", want: guardUnknown},
		{name: "EXECUTION_START without a payload, on A", seed: "none", tail: []api.Event{noPayload("x1")}, on: "a", want: guardUnknown},
		{name: "EXECUTION_START without an ExecutionID after C's, override onto B", seed: "none", tail: []api.Event{c1, {Kind: api.EventExecutionStart,
			ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0), Harness: "b"}}}, on: "b", want: guardUnknown},
		{name: "EXECUTION_START after its turn's INPUT, override onto B", seed: "none", tail: []api.Event{{Kind: api.EventInput, ExecutionID: "x1",
			Message: api.TextMessage("user", "hi")}, b1}, on: "b", want: guardUnknown},
		{name: "legacy record after a modern START, on A", seed: "none", tail: []api.Event{b2, legacyOutput}, on: "a", want: guardUnknown},
		// A record with an ExecutionID but no START after an unfinished legacy turn: the
		// controller cannot tell whether it continues that turn.
		{name: "older writer's INPUT after an unfinished legacy turn, on A", seed: "none", tail: []api.Event{legacyInput, input1}, on: "a", want: guardUnknown},
		{name: "older writer's TOOL_CALL after an unfinished legacy turn, override onto B", seed: "none", tail: []api.Event{legacyInput, legacyOutput, toolCall1}, on: "b", want: guardUnknown},
		// The malformed turn is not the one that left the compute LIVE.
		{name: "payload-less turn before A's turn, on A", seed: "none", tail: []api.Event{noPayload("x0"), turnEnd("x0"), turnStart("x1", "a"),
			{Kind: api.EventInput, ExecutionID: "x1", Message: api.TextMessage("user", "hi")}}, on: "a", want: guardUnknown},
	} {
		confirm := row
		row.name += ", dial failure"
		confirm.name += ", confirm failure"
		confirm.failConfirm = true
		rows = append(rows, row, confirm)
	}
	for _, row := range rows {
		for _, variant := range []string{"release", "unsupported", "release-error"} {
			t.Run(row.name+"/"+variant, func(t *testing.T) {
				t.Parallel()
				store, err := sqlitelog.Open(filepath.Join(t.TempDir(), "journal.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close() })
				const uid = "s"
				e := &guardEnv{t: t, ctx: t.Context(), store: store, log: &guardLog{Log: store.Session(uid)}, uid: uid}
				seed := row.seed
				if seed == "" {
					seed = "live"
				}
				seedGuardJournal(t, e.log.Log, seed, row.recorded, "")
				appendGuardEvents(t, e.log.Log, row.tail...)
				head, err := e.log.Log.Head()
				if err != nil {
					t.Fatal(err)
				}
				dial := WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
					if row.failEnd {
						return echoagent.Harness{}, func() error { return nil }, nil
					}
					return nil, nil, errGuardInjected
				})
				var trace []string
				placers := map[string]*Placer{}
				safes := map[string]*safeAllocationBackend{}
				for _, name := range []string{"a", "b", "c"} {
					var backend Backend = &allocationBackend{trace: &trace}
					switch variant {
					case "release":
						safes[name] = &safeAllocationBackend{allocationBackend: backend.(*allocationBackend), key: name}
					case "release-error":
						safes[name] = &safeAllocationBackend{allocationBackend: backend.(*allocationBackend), key: name, releaseErr: errGuardRelease}
					}
					if safe := safes[name]; safe != nil {
						backend = safe
					}
					placers[name] = New(backend, echoagent.Model, dial)
				}
				placers["a-alias"] = placers["a"]
				if _, err := NewRegistry("a", placers); err != nil {
					t.Fatal(err)
				}
				if row.peerTurn {
					e.log.onConfirm = func() {
						e.peerAppend(api.Event{Kind: api.EventExecutionStart, ExecutionID: "peer",
							ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0), Harness: "a"}})
					}
				}
				if row.failConfirm {
					e.log.confirmErr = errGuardInjected
				}
				if row.failEnd {
					e.log.onAppend = func(ev api.Event) error {
						if ev.Kind == api.EventEnd {
							return errGuardInjected
						}
						return nil
					}
				}
				opts := []ExecOption{WithHarness(row.on)}
				if row.on != "a" {
					opts = append(opts, WithOverride())
				}
				_, err = placers[row.on].Exec(e.ctx, e.log, uid, []api.Message{*api.TextMessage("user", "hi")}, head, opts...)

				if !reflect.DeepEqual(trace, []string{"create"}) {
					t.Fatalf("allocations = %v, want one create", trace)
				}
				if !errors.Is(err, errGuardInjected) {
					t.Fatalf("error = %v, want the injected cause", err)
				}
				var released, otherReleased []string
				for name, safe := range safes {
					if name == row.on {
						released = safe.released
					} else {
						otherReleased = append(otherReleased, safe.released...)
					}
				}
				if len(otherReleased) != 0 {
					t.Fatalf("released %v on the Placer that did not run the Exec", otherReleased)
				}
				var left *ComputeLeftRunningError
				typed := errors.As(err, &left)
				switch {
				case row.want == guardUnchanged:
					if typed || len(released) != 0 {
						t.Fatalf("error = %v released = %v, want the cause alone and no release", err, released)
					}
				case row.want == guardSkips:
					if !typed || !strings.Contains(left.Cleanup.Error(), "release skipped") || len(released) != 0 {
						t.Fatalf("error = %v released = %v, want a skipped release reported", err, released)
					}
				case row.want == guardUnknown:
					if !typed || !strings.Contains(left.Cleanup.Error(), "ownership unknown") || len(released) != 0 {
						t.Fatalf("error = %v released = %v, want unknown ownership reported", err, released)
					}
				case variant == "release":
					if typed || !reflect.DeepEqual(released, []string{uid}) {
						t.Fatalf("error = %v released = %v, want the cause after exactly one release", err, released)
					}
				case variant == "unsupported":
					if !typed || !errors.Is(err, ErrComputeLeftRunning) || left.Instance != "resource-"+uid ||
						!strings.Contains(left.Cleanup.Error(), "does not implement CleanupRuntime") {
						t.Fatalf("error = %v, want ErrComputeLeftRunning naming the instance", err)
					}
				case variant == "release-error":
					if !typed || !errors.Is(left.Cleanup, errGuardRelease) || !reflect.DeepEqual(released, []string{uid}) {
						t.Fatalf("error = %v released = %v, want one failed release reported", err, released)
					}
				}
			})
		}
	}
}

// TestResumeMarkerAfterARedrivenTurn pins what liveOwner reads from a LIFECYCLE_RESUME: a
// Registry.Resume that re-drives a pending turn recorded on B runs it on B and journals the turn's
// END before the RESUME, so a turn record directly precedes it. One with nothing pending runs on
// the session's own harness, A, and appends the RESUME alone, so after a completed turn on B the
// journal reads the same either way: the own harness or B.
func TestResumeMarkerAfterARedrivenTurn(t *testing.T) {
	for _, row := range []struct {
		seed      string
		restoreOn string
	}{
		{seed: "pending", restoreOn: "b"},
		{seed: "live", restoreOn: "a"},
	} {
		t.Run(row.seed, func(t *testing.T) {
			store, err := sqlitelog.Open(filepath.Join(t.TempDir(), "journal.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			log := store.Session("s")
			seedGuardJournal(t, log, row.seed, "b", "")
			dial := WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
				return echoagent.Harness{}, func() error { return nil }, nil
			})
			traces := map[string]*[]string{}
			placers := map[string]*Placer{}
			for _, name := range []string{"a", "b"} {
				traces[name] = &[]string{}
				placers[name] = New(&allocationBackend{trace: traces[name]}, echoagent.Model, dial)
			}
			r, err := NewRegistry("a", placers)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Resume(t.Context(), log, "s", "a"); err != nil {
				t.Fatal(err)
			}
			for name, trace := range traces {
				want := []string{}
				if name == row.restoreOn {
					want = []string{"restore"}
				}
				if !reflect.DeepEqual(*trace, want) {
					t.Fatalf("%s allocations = %v, want %v", name, *trace, want)
				}
			}
			recs, err := log.Read(1)
			if err != nil {
				t.Fatal(err)
			}
			last, prev := recs[len(recs)-1].Event, recs[len(recs)-2].Event
			if last.Kind != api.EventLifecycle || last.Lifecycle.Kind != api.LifecycleResume {
				t.Fatalf("last record is %v, want RESUME", last.Kind)
			}
			if prev.Kind != api.EventEnd || prev.ExecutionID != "x1" {
				t.Fatalf("record before RESUME is %v of %q, want the turn's END", prev.Kind, prev.ExecutionID)
			}
			if recorded, others := liveOwnerOf(t, recs); recorded != "" || !slices.Equal(slices.Collect(others), []string{"b"}) {
				t.Fatalf("liveOwner = %q, %q, want the own harness or B", recorded, slices.Collect(others))
			}
		})
	}
}

// TestStaleResumePickAppendsASecondResume shows a RESUME that follows a RESUME on another harness:
// two Registry.Resume calls without expected_last_seq, on hosts sharing the journal, both pick B
// for a turn pending on B. The first re-drives it and appends END and RESUME; the second, its pick
// now stale, restores on B, finds nothing pending and appends a second RESUME, leaving B's compute
// running. A failed Exec then cannot tell whether that RESUME ran on A or on B, so it reports the
// compute on either harness instead of releasing it (B) or keeping it as the journal's (A).
func TestStaleResumePickAppendsASecondResume(t *testing.T) {
	for _, on := range []string{"a", "b"} {
		t.Run("exec on "+on, func(t *testing.T) {
			store, err := sqlitelog.Open(filepath.Join(t.TempDir(), "journal.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			const uid = "s"
			seedGuardJournal(t, store.Session(uid), "pending", "b", "")
			var dialFails atomic.Bool
			dial := WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
				if dialFails.Load() {
					return nil, nil, errGuardInjected
				}
				return echoagent.Harness{}, func() error { return nil }, nil
			})
			// The hosts share each harness's backend, as hosts sharing a journal share its compute.
			traces := map[string]*[]string{}
			backends := map[string]*safeAllocationBackend{}
			for _, name := range []string{"a", "b"} {
				traces[name] = &[]string{}
				backends[name] = &safeAllocationBackend{allocationBackend: &allocationBackend{trace: traces[name]}, key: name}
			}
			host := func() (*Registry, map[string]*Placer) {
				placers := map[string]*Placer{}
				for name, backend := range backends {
					placers[name] = New(backend, echoagent.Model, dial)
				}
				r, err := NewRegistry("a", placers)
				if err != nil {
					t.Fatal(err)
				}
				return r, placers
			}
			host1, placers1 := host()
			host2, _ := host()

			resolved, proceed := make(chan string, 1), make(chan struct{})
			done := make(chan error, 1)
			go func() {
				done <- host2.Resume(t.Context(), store.Session(uid), uid, "a", WithResolvedHarnessCheck(func(name string) error {
					resolved <- name
					<-proceed
					return nil
				}))
			}()
			if name := <-resolved; name != "b" {
				close(proceed)
				t.Fatalf("host 2 resolved %q, want the pending turn's b", name)
			}
			if err := host1.Resume(t.Context(), store.Session(uid), uid, "a"); err != nil {
				close(proceed)
				t.Fatal(err)
			}
			close(proceed)
			if err := <-done; err != nil {
				t.Fatalf("host 2 Resume = %v, want its stale pick to run", err)
			}
			if want := []string{"restore", "restore"}; !reflect.DeepEqual(*traces["b"], want) || len(*traces["a"]) != 0 {
				t.Fatalf("allocations a=%v b=%v, want b=%v", *traces["a"], *traces["b"], want)
			}
			recs, err := store.Session(uid).Read(1)
			if err != nil {
				t.Fatal(err)
			}
			var tail []string
			for _, r := range recs[len(recs)-3:] {
				kind := string(r.Event.Kind)
				if r.Event.Lifecycle != nil {
					kind = string(r.Event.Lifecycle.Kind)
				}
				tail = append(tail, kind)
			}
			if want := []string{string(api.EventEnd), string(api.LifecycleResume), string(api.LifecycleResume)}; !reflect.DeepEqual(tail, want) {
				t.Fatalf("journal tail = %v, want %v", tail, want)
			}

			dialFails.Store(true)
			opts := []ExecOption{WithHarness(on)}
			if on != "a" {
				opts = append(opts, WithOverride())
			}
			_, err = placers1[on].Exec(t.Context(), store.Session(uid), uid, []api.Message{*api.TextMessage("user", "hi")},
				recs[len(recs)-1].Seq, opts...)
			var left *ComputeLeftRunningError
			if !errors.As(err, &left) || !errors.Is(err, errGuardInjected) || !strings.Contains(left.Cleanup.Error(), "ownership unknown") {
				t.Fatalf("Exec = %v, want the dial failure with unknown ownership reported", err)
			}
			for name, backend := range backends {
				if len(backend.released) != 0 {
					t.Fatalf("released %v on %s, want no release", backend.released, name)
				}
			}
		})
	}
}

// ownershipEvent ignores exactly the receipt CloseApproval writes; every other receipt, including
// one with the same code but not CloseApproval's shape, moves the compute state as SES-8 does.
func TestOwnershipEvent(t *testing.T) {
	receipt := func(code api.ToolResultCode, isError bool, decisionSeq int64) api.Event {
		return api.Event{Kind: api.EventToolResult, Result: &api.ToolResult{ID: "c1", IsError: isError, Code: code,
			ApprovalRequestSeq: 3, ApprovalDecisionSeq: decisionSeq}}
	}
	for _, tc := range []struct {
		name  string
		ev    api.Event
		moves bool
	}{
		{name: "not executed", ev: receipt(api.ToolResultCodeNotExecuted, true, 4)},
		{name: "outcome unknown", ev: receipt(api.ToolResultCodeOutcomeUnknown, true, 4)},
		{name: "not executed without is_error", ev: receipt(api.ToolResultCodeNotExecuted, false, 4), moves: true},
		{name: "outcome unknown without a decision", ev: receipt(api.ToolResultCodeOutcomeUnknown, true, 0), moves: true},
		{name: "executor error", ev: receipt(api.ToolResultCodeExecutorError, true, 4), moves: true},
		{name: "approval denied", ev: receipt(api.ToolResultCodeApprovalDenied, true, 4), moves: true},
		{name: "executor success", ev: receipt(api.ToolResultCodeUnspecified, false, 4), moves: true},
		{name: "tool result without a result", ev: api.Event{Kind: api.EventToolResult}, moves: true},
		{name: "approval request", ev: api.Event{Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "c1"}}},
		{name: "suspend", ev: suspendEvent, moves: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, moves := ownershipEvent(tc.ev)
			if moves != tc.moves {
				t.Fatalf("ownershipEvent moves = %v, want %v", moves, tc.moves)
			}
			if want, wantMoves := computestate.Event(tc.ev); tc.moves && (state != want || !wantMoves) {
				t.Fatalf("ownershipEvent = %v, want computestate.Event's %v", state, want)
			}
		})
	}
}

// A CloseApproval receipt after a RESUME says nothing about where compute runs, so liveOwner
// attributes LIVE to the RESUME (the session's own harness or a pending turn's) rather than to the
// closed turn.
func TestLiveOwnerSkipsCloseApprovalReceipt(t *testing.T) {
	events := []api.Event{
		{Kind: api.EventExecutionStart, ExecutionID: "x1", ExecutionStart: &api.ExecutionStart{Harness: "b"}},
		{Kind: api.EventToolCall, ExecutionID: "x1", ToolCall: &api.ToolCall{ID: "c1", Tool: "write"}},
		{Kind: api.EventApprovalRequest, ExecutionID: "x1", Approval: &api.ApprovalRequest{ToolCallID: "c1"}},
		{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleResume}},
		{Kind: api.EventApprovalResult, ExecutionID: "x1", ApprovalResult: &api.ApprovalResult{ToolCallID: "c1", RequestSeq: 3}},
		{Kind: api.EventToolResult, ExecutionID: "x1", Result: &api.ToolResult{ID: "c1", IsError: true,
			Code: api.ToolResultCodeNotExecuted, ApprovalRequestSeq: 3, ApprovalDecisionSeq: 5}},
	}
	recs := make([]eventlog.Record, len(events))
	for i, ev := range events {
		recs[i] = eventlog.Record{Seq: int64(i + 1), Event: ev}
	}
	if recorded, others := liveOwnerOf(t, recs); recorded != "" || !slices.Equal(slices.Collect(others), []string{"b"}) {
		t.Fatalf("liveOwner = %q, %q, want the own harness or B", recorded, slices.Collect(others))
	}
	if state := ownershipState(recs); state != api.ComputeLive {
		t.Fatalf("ownershipState = %v, want LIVE from the RESUME", state)
	}
}

func turnStart(id, harness string) api.Event {
	return api.Event{Kind: api.EventExecutionStart, ExecutionID: id,
		ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0), Harness: harness}}
}

func turnEnd(id string) api.Event {
	return api.Event{Kind: api.EventEnd, ExecutionID: id, End: &api.HarnessEnd{State: "COMPLETED"}}
}

// liveOwnerOf is liveOwner over executionHarnesses of recs, which must attribute every turn.
func liveOwnerOf(t *testing.T, recs []eventlog.Record) (string, iter.Seq[string]) {
	t.Helper()
	harnesses, known := executionHarnesses(recs)
	if !known {
		t.Fatal("executionHarnesses: known = false, want true")
	}
	return liveOwner(recs, harnesses)
}

// TestLiveOwnerByExecutionID pins which harness the guard attributes a turn record to
// (executionHarnesses, liveOwner, resumeCandidates), in two groups.
//
// Reachable shapes, which the controller writes or accepts: a record belongs to the
// EXECUTION_START with its ExecutionID, wherever that START is (before a FORK marker for a fork
// child's inherited turn, before another turn's START when executions interleave). A record with no
// ExecutionID is a legacy (v0.1.2) turn's, and one whose ExecutionID has no EXECUTION_START is an
// older writer's; both ran on the session's own harness ("").
//
// Malformed fixtures, which the controller rejects but the hash chain does not catch: the journal
// does not attribute every turn to one harness, wherever the malformed record is, so
// executionHarnesses reports known false and the guard reports the compute as of unknown ownership
// without asking liveOwner. A repeated START naming the same harness is rejected too, but still
// names the turn's harness, so it is in the first group.
func TestLiveOwnerByExecutionID(t *testing.T) {
	resume := api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleResume}}
	fork := api.Event{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleFork}}
	c1, b1, b2 := turnStart("x1", "c"), turnStart("x1", "b"), turnStart("x2", "b")
	e1, e2 := turnEnd("x1"), turnEnd("x2")
	input1 := api.Event{Kind: api.EventInput, ExecutionID: "x1", Message: api.TextMessage("user", "hi")}
	noPayload := func(id string) api.Event { return api.Event{Kind: api.EventExecutionStart, ExecutionID: id} }
	noID := api.Event{Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(0), Harness: "b"}}
	legacyInput := api.Event{Kind: api.EventInput, Message: api.TextMessage("user", "hi")}
	legacyEnd := api.Event{Kind: api.EventEnd, End: &api.HarnessEnd{State: "COMPLETED"}}
	legacyOutput := api.Event{Kind: api.EventOutput, Message: api.TextMessage("assistant", "hi")}
	legacyError := api.Event{Kind: api.EventError, Err: &api.Error{Description: "failed"}}
	toolCall1 := api.Event{Kind: api.EventToolCall, ExecutionID: "x1", ToolCall: &api.ToolCall{ID: "c1", Tool: "write"}}
	// An APPROVAL_REQUEST moves no compute state, and no legacy writer recorded one.
	idlessApproval := api.Event{Kind: api.EventApprovalRequest, Approval: &api.ApprovalRequest{ToolCallID: "c1"}}
	records := func(events []api.Event) []eventlog.Record {
		recs := make([]eventlog.Record, len(events))
		for i, ev := range events {
			recs[i] = eventlog.Record{Seq: int64(i + 1), Event: ev}
		}
		return recs
	}
	for _, tc := range []struct {
		name     string
		events   []api.Event
		recorded string
		others   []string
	}{
		{name: "sequential turns", events: []api.Event{c1, e1, b2, e2}, recorded: "b"},
		{name: "interleaved, earlier turn ends last", events: []api.Event{c1, b2, e2, e1}, recorded: "c"},
		{name: "interleaved, earlier turn ends before the later one", events: []api.Event{c1, b2, e1}, recorded: "c"},
		{name: "interleaved, later turn ends last", events: []api.Event{c1, b2, e1, e2}, recorded: "b"},
		{name: "interleaved, peer SUSPEND before the earlier turn's END", events: []api.Event{c1, b2, suspendEvent, e1}, recorded: "c"},
		{name: "interleaved, earlier turn re-driven across FORK", events: []api.Event{c1, b2, fork, e1}, recorded: "c"},
		{name: "inherited turn re-driven across FORK", events: []api.Event{b2, fork, e2}, recorded: "b"},
		{name: "legacy turn, then a modern one", events: []api.Event{legacyInput, legacyEnd, b2, e2}, recorded: "b"},
		{name: "legacy turn", events: []api.Event{legacyInput, legacyOutput, legacyEnd}, recorded: ""},
		{name: "unfinished legacy turn, then a modern one", events: []api.Event{legacyInput, b2, e2}, recorded: "b"},
		{name: "legacy turn, then an older writer's turn", events: []api.Event{legacyInput, legacyEnd, resume, input1}, recorded: ""},
		{name: "failed legacy turn, then an older writer's turn", events: []api.Event{legacyInput, legacyError, input1}, recorded: ""},
		{name: "older writer's turn without EXECUTION_START", events: []api.Event{b2, e2, turnEnd("x0")}, recorded: ""},
		{name: "repeated EXECUTION_START naming the same harness", events: []api.Event{b1, b1}, recorded: "b"},
		{name: "repeated EXECUTION_START naming the same harness after its INPUT", events: []api.Event{b1, input1, b1, e1}, recorded: "b"},
		{name: "fork child, nothing re-driven, resumed", events: []api.Event{b2, e2, fork, resume}},
		{name: "inherited turn re-driven across FORK, resumed", events: []api.Event{b2, fork, e2, resume}, others: []string{"b"}},
		{name: "interleaved, earlier turn re-driven across FORK, resumed", events: []api.Event{c1, b2, fork, e1, resume}, others: []string{"c"}},
		{name: "interleaved, both re-driven across FORK, resumed", events: []api.Event{c1, b2, fork, e1, e2, resume}, others: []string{"c", "b"}},
		{name: "interleaved, peer SUSPEND, earlier turn re-driven across FORK, resumed",
			events: []api.Event{c1, b2, fork, suspendEvent, e1, resume}, others: []string{"c"}},
		{name: "interleaved without FORK, resumed", events: []api.Event{c1, b2, e1, resume}, others: []string{"c", "b"}},
		{name: "legacy turn and a modern one, resumed", events: []api.Event{legacyInput, legacyEnd, b2, e2, resume}, others: []string{"", "b"}},
		{name: "older writer's turn after a modern one, resumed", events: []api.Event{b2, e2, turnEnd("x0"), resume}, others: []string{"b", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorded, others := liveOwnerOf(t, records(tc.events))
			if got := slices.Collect(others); recorded != tc.recorded || !slices.Equal(got, tc.others) {
				t.Fatalf("liveOwner = %q, %q, want %q, %q", recorded, got, tc.recorded, tc.others)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		events []api.Event
	}{
		{name: "conflicting EXECUTION_START at the tail", events: []api.Event{c1, b1}},
		{name: "conflicting EXECUTION_START at the tail, resumed", events: []api.Event{c1, b1, resume}},
		{name: "conflicting EXECUTION_START before the turn's END", events: []api.Event{c1, b1, e1}},
		{name: "conflicting EXECUTION_START across FORK", events: []api.Event{c1, fork, b1, e1}},
		{name: "conflicting EXECUTION_START naming an alias", events: []api.Event{turnStart("x1", "a"), turnStart("x1", "a-alias")}},
		{name: "EXECUTION_START without a payload", events: []api.Event{noPayload("x1")}},
		{name: "EXECUTION_START without a payload, resumed", events: []api.Event{noPayload("x1"), resume}},
		{name: "EXECUTION_START without a payload, then a START", events: []api.Event{noPayload("x1"), b1}},
		{name: "EXECUTION_START without an ExecutionID", events: []api.Event{c1, noID}},
		{name: "EXECUTION_START without an ExecutionID first", events: []api.Event{noID, legacyOutput}},
		{name: "EXECUTION_START after its turn's INPUT", events: []api.Event{input1, b1}},
		{name: "EXECUTION_START after its turn's INPUT, resumed", events: []api.Event{input1, b1, e1, resume}},
		{name: "legacy record after a modern START", events: []api.Event{b2, legacyOutput}},
		{name: "legacy record after a modern START, resumed", events: []api.Event{b2, legacyOutput, resume}},
		{name: "legacy record after an older writer's record", events: []api.Event{turnEnd("x0"), legacyOutput}},
		{name: "legacy record after a modern START across FORK", events: []api.Event{b2, fork, legacyOutput}},
		{name: "older writer's record after an unfinished legacy turn", events: []api.Event{legacyInput, input1}},
		{name: "older writer's record after an unfinished legacy turn and a RESUME", events: []api.Event{legacyInput, legacyOutput, resume, toolCall1}},
		{name: "record no legacy writer wrote without an ExecutionID", events: []api.Event{legacyInput, idlessApproval, legacyEnd}},
		{name: "malformed turn before the turn that decides", events: []api.Event{noPayload("x0"), turnEnd("x0"), turnStart("x1", "a"), input1}},
		{name: "malformed record after the turn that decides", events: []api.Event{b2, e2, idlessApproval}},
	} {
		t.Run("malformed/"+tc.name, func(t *testing.T) {
			if _, known := executionHarnesses(records(tc.events)); known {
				t.Fatal("executionHarnesses: known = true, want false")
			}
		})
	}
}
