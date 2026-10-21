package session_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/agent-primitives/sessions/api"
	v1 "github.com/agent-primitives/sessions/api/genpb"
	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/runtime/substrate"
	"github.com/agent-primitives/sessions/sqlitelog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Control-plane evidence modes injected by restoreControl.
const (
	modeReadFailure int32 = iota + 1
	modePending           // newest durable copy still uploading
	modeLocalOnly         // newest checkpoint has no durable copy (a paused actor's local checkpoint)
	modeTransitioning
	modeTerminatedPending // a crashed actor retains an upload that will never complete
	modeOmitUID           // GetActor omits the actor UID; ResumeActor still reports it
)

// Inject only the control-plane read/readiness evidence, preserving actor lifecycle side effects.
type restoreControl struct {
	*actorControl
	mode atomic.Int32
}

func (c *restoreControl) GetActor(ctx context.Context, ref substrate.ActorRef) (substrate.ActorInfo, error) {
	mode := c.mode.Load()
	if mode == modeReadFailure {
		return substrate.ActorInfo{}, errors.New("control unavailable")
	}
	info, err := c.actorControl.GetActor(ctx, ref)
	switch mode {
	case modeTransitioning:
		info.Status = substrate.StatusTransitioning
	case modeTerminatedPending:
		info.Status = substrate.StatusTerminated
	case modeOmitUID:
		info.UID = ""
	}
	info.SnapshotNotReady = mode == modePending || mode == modeLocalOnly || mode == modeTerminatedPending
	info.SnapshotPending = mode == modePending || mode == modeTerminatedPending
	return info, err
}

func (c *restoreControl) SuspendActor(ctx context.Context, ref substrate.ActorRef) (substrate.ActorInfo, error) {
	info, err := c.actorControl.SuspendActor(ctx, ref)
	mode := c.mode.Load()
	info.SnapshotNotReady = mode == modePending || mode == modeLocalOnly
	info.SnapshotPending = mode == modePending
	return info, err
}

type heldRestoreHost struct {
	c       v1.SessionsClient
	ctl     *restoreControl
	store   *sqlitelog.Store
	resumes atomic.Int32
	dials   atomic.Int32
	dialErr atomic.Int32 // dials still to fail
}

func newHeldRestoreHost(t *testing.T, memory bool) *heldRestoreHost {
	t.Helper()
	h := &heldRestoreHost{}
	h.ctl = &restoreControl{actorControl: &actorControl{
		status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{},
		onResume: func(string) { h.resumes.Add(1) },
	}}
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	h.store = store
	desc, err := echoagent.Harness{}.Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if memory {
		desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
	}
	backend := substrate.New(h.ctl, "space", substrate.ObjectRef{Name: "echo"}, desc)
	p := placement.New(backend, echoagent.Model, placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		h.dials.Add(1)
		if h.dialErr.Add(-1) >= 0 {
			return nil, nil, errors.New("dial failed")
		}
		return echoagent.Harness{}, func() error { return nil }, nil
	}))
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p})
	if err != nil {
		t.Fatal(err)
	}
	h.c = serveRegistry(t, store, r)
	return h
}

// Resume always passes the last SUSPEND ref, so after any Suspend, Resume of the RUNNING actor
// must stay a no-op: a repeated Resume, a Resume retried after its dial failed, and the Resume
// that re-drives an interrupted turn. Memory tier keeps the recorded-UID replacement guard.
func TestResumeOfRunningActorAfterSuspend(t *testing.T) {
	for _, tier := range []string{"stateless", "memory"} {
		for _, scenario := range []string{"repeated Resume", "dial retry", "interrupted turn", "running after Exec", "replaced actor", "omitted UID replaced"} {
			if strings.Contains(scenario, "replaced") && tier != "memory" {
				continue
			}
			t.Run(tier+"/"+scenario, func(t *testing.T) {
				h := newHeldRestoreHost(t, tier == "memory")
				c := h.c
				sess := mustCreate(t, c)
				if err := execHi(c, sess); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Suspend(t.Context(), &v1.SuspendRequest{Session: sess}); err != nil {
					t.Fatal(err)
				}
				if scenario == "dial retry" {
					h.dialErr.Store(1)
				}
				_, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: sess})
				if scenario == "dial retry" {
					if err == nil {
						t.Fatal("resume with a failing dial succeeded")
					}
				} else if err != nil {
					t.Fatal(err)
				}
				log := h.store.Session(sess)
				switch scenario {
				case "running after Exec":
					if err := execHi(c, sess); err != nil {
						t.Fatal(err)
					}
				case "interrupted turn":
					// The durable prefix of a turn whose host crashed after the harness was reached.
					head, err := log.Head()
					if err != nil {
						t.Fatal(err)
					}
					fence, err := log.NewFence()
					if err != nil {
						t.Fatal(err)
					}
					for i, event := range []api.Event{
						{Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{InputCount: proto.Int64(1)}},
						{Kind: api.EventInput, Message: api.TextMessage("user", "again")},
					} {
						event.ExecutionID = "interrupted"
						if _, err := log.Append(head+int64(i), fence, event); err != nil {
							t.Fatal(err)
						}
					}
				case "replaced actor", "omitted UID replaced":
					h.ctl.mu.Lock()
					h.ctl.uids[sess] = "replacement-uid"
					h.ctl.mu.Unlock()
					if scenario == "omitted UID replaced" {
						h.ctl.mode.Store(modeOmitUID)
					}
				}
				head, err := log.Head()
				if err != nil {
					t.Fatal(err)
				}
				resumes, dials := h.resumes.Load(), h.dials.Load()
				_, err = c.Resume(t.Context(), &v1.ResumeRequest{Session: sess})
				if scenario == "replaced actor" {
					if status.Code(err) != codes.FailedPrecondition || h.resumes.Load() != resumes || h.dials.Load() != dials {
						t.Fatalf("resume of a replaced RUNNING actor: %v (resumes %d→%d, dials %d→%d), want FAILED_PRECONDITION before resume",
							err, resumes, h.resumes.Load(), dials, h.dials.Load())
					}
					if got, err := log.Head(); err != nil || got != head {
						t.Fatalf("refusal changed head: %d err=%v, want %d", got, err, head)
					}
					return
				}
				if scenario == "omitted UID replaced" {
					// Missing preflight evidence proceeds to the no-op resume; the controller's
					// recorded ComputeInstance gate then refuses before any turn record.
					if status.Code(err) != codes.FailedPrecondition || !strings.Contains(status.Convert(err).Message(), "cannot be reconstructed") {
						t.Fatalf("resume reporting another UID: %v, want the controller's FAILED_PRECONDITION", err)
					}
					if got, err := log.Head(); err != nil || got != head {
						t.Fatalf("refusal changed head: %d err=%v, want %d", got, err, head)
					}
					return
				}
				if err != nil {
					t.Fatalf("resume of the RUNNING actor: %v", err)
				}
				if got, err := log.Head(); scenario == "interrupted turn" && (err != nil || got <= head) {
					t.Fatalf("interrupted turn was not re-driven: head %d→%d err=%v", head, got, err)
				}
				if err := log.Verify(); err != nil {
					t.Fatal(err)
				}
				if err := execHi(c, sess); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// Refusals of a held ref happen before ResumeActor, dial or journal effects, with the class the
// condition calls for: busy is ABORTED, a pending upload is UNAVAILABLE, a stale or local-only
// checkpoint is FAILED_PRECONDITION, and a failed control read remains INTERNAL.
func TestHeldRestoreRefusalsDoNotJournal(t *testing.T) {
	for _, tc := range []struct {
		reason string
		mode   int32
		want   codes.Code
		// exec, suspend and memory-tier fork also hit the refusal through Create's attachment and
		// capture (a stateful fork captures the parent).
		exec, suspend, fork bool
	}{
		{reason: "transitioning", mode: modeTransitioning, want: codes.Aborted, exec: true},
		{reason: "terminated pending", mode: modeTerminatedPending, want: codes.FailedPrecondition},
		{reason: "wrong URI", want: codes.FailedPrecondition},
		{reason: "read failure", mode: modeReadFailure, want: codes.Internal, exec: true},
		{reason: "pending", mode: modePending, want: codes.Unavailable, exec: true, suspend: true, fork: true},
		{reason: "local-only", mode: modeLocalOnly, want: codes.FailedPrecondition, exec: true, suspend: true, fork: true},
	} {
		for _, tier := range []string{"stateless", "memory"} {
			t.Run(tier+"/"+tc.reason, func(t *testing.T) {
				h := newHeldRestoreHost(t, tier == "memory")
				c, ctl := h.c, h.ctl
				sess := mustCreate(t, c)
				if err := execHi(c, sess); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Suspend(t.Context(), &v1.SuspendRequest{Session: sess}); err != nil {
					t.Fatal(err)
				}
				if tc.reason == "wrong URI" {
					ctl.mu.Lock()
					ctl.snapshots[sess] = "newer-durable-checkpoint"
					ctl.mu.Unlock()
				}
				ctl.mode.Store(tc.mode)
				head, err := h.store.Session(sess).Head()
				if err != nil {
					t.Fatal(err)
				}
				beforeResume, beforeDial := h.resumes.Load(), h.dials.Load()
				if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: sess}); status.Code(err) != tc.want {
					t.Fatalf("resume: %v, want %s", err, tc.want)
				}
				if tc.exec {
					if err := execHi(c, sess); status.Code(err) != tc.want {
						t.Fatalf("exec: %v, want %s", err, tc.want)
					}
				}
				if tc.suspend {
					if _, err := c.Suspend(t.Context(), &v1.SuspendRequest{Session: sess}); status.Code(err) != tc.want {
						t.Fatalf("suspend: %v, want %s", err, tc.want)
					}
				}
				if tc.fork && tier == "memory" {
					if err := forkParent(c, sess); status.Code(err) != tc.want {
						t.Fatalf("fork: %v, want %s", err, tc.want)
					}
				}
				if h.resumes.Load() != beforeResume || h.dials.Load() != beforeDial {
					t.Fatal("refusal resumed compute or dialed the harness")
				}
				if got, err := h.store.Session(sess).Head(); err != nil || got != head {
					t.Fatalf("refusal changed head: %d err=%v, want %d", got, err, head)
				}
				if err := h.store.Session(sess).Verify(); err != nil {
					t.Fatal(err)
				}
				ctl.mode.Store(0)
				// Capture the current state explicitly, rather than retrying the stale held ref.
				if _, err := c.Suspend(t.Context(), &v1.SuspendRequest{Session: sess}); err != nil {
					t.Fatal(err)
				}
				if _, err := c.Resume(t.Context(), &v1.ResumeRequest{Session: sess}); err != nil {
					t.Fatal(err)
				}
				if err := execHi(c, sess); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
