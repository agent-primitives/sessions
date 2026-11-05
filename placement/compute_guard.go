package placement

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strconv"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/controller"
	"github.com/agent-primitives/sessions/eventlog"
	"github.com/agent-primitives/sessions/internal/computestate"
	"github.com/agent-primitives/sessions/observability"
)

// ErrComputeLeftRunning is matched (errors.Is) by the error of a Resume or Exec that failed after
// the runtime returned compute, when the journal records nothing that owns that compute and the
// host could not release it. The error is a *ComputeLeftRunningError.
var ErrComputeLeftRunning = errors.New("placement: compute instance may still be running")

// ComputeLeftRunningError reports compute that a failed Resume or Exec may have left running. The
// runtime returned the compute, a later step failed, and the journal, read after the failure, does
// not show the operation's harness owning that compute (the guard's own fold of the records, which
// unlike the stored SES-8 compute_state skips a CloseApproval receipt). The host never calls Stop
// on this path (RT-7) and did not release the compute either: the backend has no
// api.CleanupRuntime, the release failed, the journal could not be read or verified (hash chain) or
// is shorter than it was before the operation, the journal does not record whether the latest
// resume ran on this harness, or another owner (a writer that committed during the operation, a
// checkpoint, a newer fence, a deletion) may hold it, so a release could pull it from under them.
//
// The error the operation failed with is Cause: errors.Is and errors.As reach it through Unwrap,
// so its classification (and the gRPC code session.Service maps it to) is unchanged. Cleanup says
// why nothing was released, and is reachable only through errors.As, never errors.Is: a cleanup
// failure such as a release deadline must not change how Cause is classified. The message carries
// Cleanup's fixed text only, never the text of a journal or release error it wraps.
type ComputeLeftRunningError struct {
	// Instance is the Incarnation.ID the runtime returned. A backend that reattaches a session
	// to the same compute on retry (substrate: the actor named by the session UID) returns the
	// same ID.
	Instance string
	// ComputeInstance is the Incarnation.ComputeInstance the runtime returned, empty for a backend
	// that does not tell instances apart behind one ID.
	ComputeInstance string
	// Cause is the error the operation failed with.
	Cause error
	// Cleanup is why the host did not release the compute.
	Cleanup error
}

// Error reports Cause first, so a message read by prefix reads as it did without the report.
func (e *ComputeLeftRunningError) Error() string {
	instance := strconv.Quote(e.Instance)
	if e.ComputeInstance != "" {
		instance += " (uid " + strconv.Quote(e.ComputeInstance) + ")"
	}
	return fmt.Sprintf("%v; compute instance %s may still be running: %v", e.Cause, instance, e.Cleanup)
}

// Unwrap returns Cause only; Cleanup stays out of the errors.Is chain.
func (e *ComputeLeftRunningError) Unwrap() error { return e.Cause }

// Is reports whether target is ErrComputeLeftRunning.
func (e *ComputeLeftRunningError) Is(target error) bool { return target == ErrComputeLeftRunning }

// releaseTimeout bounds the release of compute a failed Resume or Exec left unowned. The release
// ignores the caller's cancellation, so a turn the caller abandoned is still cleaned up, and runs
// under the session guard, which a wedged release therefore holds for at most this long.
const releaseTimeout = 30 * time.Second

// computeGuard decides what happens to compute a Resume or Exec allocated when the operation then
// fails. The caller arms it inside the allocation effect once the runtime returns compute and
// settles it in a defer registered before the allocation, so every early return after the effect
// passes through settle. handoffPark disarms it once its SUSPEND commits the compute to the
// approval gate; a handoff that fails before then leaves it armed.
type computeGuard struct {
	p          *Placer
	log        eventlog.Store
	sessionUID string
	// headBefore is the journal head read before the allocation. A record after it was written
	// during the operation, by the controller or by another owner; a journal that ends before it
	// lost records.
	headBefore int64
	// override is set when the operation runs on a Placer other than the session's own (an
	// ExecRequest.harness override, or the recovery of a turn recorded on one).
	override bool
	armed    bool
	inc      api.Incarnation
}

func (g *computeGuard) arm(inc api.Incarnation) { g.armed, g.inc = true, inc }

func (g *computeGuard) disarm() { g.armed = false }

// settle runs when the operation returns. On success, or when no compute was returned, it does
// nothing. Otherwise it reads and verifies the journal: compute the journal owns (ownership) is
// left to it; compute another owner may hold, or whose owner the journal cannot establish (it
// fails verification, is shorter than before the operation, or does not record which harness left
// the compute running), is reported without a release; and unowned compute is released through
// api.CleanupRuntime or, failing that, reported as a *ComputeLeftRunningError wrapping *errp.
func (g *computeGuard) settle(ctx context.Context, errp *error) {
	err := *errp
	if err == nil || !g.armed {
		return
	}
	left := &ComputeLeftRunningError{Instance: g.inc.ID, ComputeInstance: g.inc.ComputeInstance, Cause: err}
	// Store.Read takes no context, so a cancelled caller still gets an answer.
	recs, rerr := g.log.Read(1)
	if rerr != nil {
		left.Cleanup = &fixedTextError{msg: "ownership unknown: the journal could not be read", err: rerr}
		g.report(ctx, errp, left)
		return
	}
	// Records that fail verification are no evidence either way: a tampered SUSPEND can read as a
	// RESUME that owns the compute, and a tampered turn as nothing that does.
	if verr := eventlog.VerifyChain("", recs); verr != nil {
		left.Cleanup = &fixedTextError{msg: "ownership unknown: the journal could not be verified", err: verr}
		g.report(ctx, errp, left)
		return
	}
	// The chain verifies from genesis only, so a journal that lost its tail still verifies, and its
	// prefix can read LIVE on this harness (a COLD session's final SUSPEND lost). headBefore shows
	// the loss.
	if n := len(recs); g.headBefore > 0 && (n == 0 || recs[n-1].Seq < g.headBefore) {
		left.Cleanup = errors.New("ownership unknown: the journal is shorter than it was before the operation")
		g.report(ctx, errp, left)
		return
	}
	moved := len(recs) > 0 && recs[len(recs)-1].Seq > g.headBefore
	// The journal as read now decides: the runtime attached compute the journal owned and nothing
	// committed since changed that, or the controller journaled this turn on it. Ownership before
	// the operation does not count once a record committed during it says otherwise: after another
	// host's SUSPEND the runtime may have woken compute the journal no longer owns, which the moved
	// case below reports.
	switch g.ownership(recs) {
	case owned:
		return
	case ownerUnknown:
		left.Cleanup = errors.New("ownership unknown: the journal does not record which harness left the compute running")
		g.report(ctx, errp, left)
		return
	}
	switch {
	case moved:
		// Another writer (Suspend, Delete, an approval decision, a peer's turn) committed during
		// the operation. A takeover by a caller's own stale cursor moves nothing and is not this.
		left.Cleanup = fmt.Errorf("release skipped: the journal moved past seq %d during the operation", g.headBefore)
	case errors.Is(err, ErrCheckpointing):
		// Including a checkpoint that superseded this turn (errSuperseded): a stateful Fork may
		// still be snapshotting the compute, or may fail before it suspends it, and only the
		// Fork's SUSPEND record would show which.
		left.Cleanup = errors.New("release skipped: a checkpoint of the session may hold it")
	case errors.Is(err, eventlog.ErrFenced):
		left.Cleanup = errors.New("release skipped: a newer writer fenced this operation")
	case errors.Is(err, eventlog.ErrDeleted):
		left.Cleanup = errors.New("release skipped: the session is deleted")
	default:
		left.Cleanup = g.release(ctx)
		if left.Cleanup == nil {
			return
		}
	}
	g.report(ctx, errp, left)
}

// report replaces *errp with left and logs it at Warn: the client sees only left's message, and an
// operator reading the log learns which instance may still be running, by the same incarnation and
// compute instance the message names. The log carries Cleanup's fixed text and the cause's
// error_kind, never an error's text (docs/observability.md).
func (g *computeGuard) report(ctx context.Context, errp *error, left *ComputeLeftRunningError) {
	*errp = left
	observability.Logger(ctx, g.p.logger).WarnContext(ctx, "compute may still be running",
		"component", "placement",
		"operation", "settle_compute",
		"session_uid", g.sessionUID,
		"incarnation_id", left.Instance,
		"compute_instance", left.ComputeInstance,
		"cleanup", left.Cleanup.Error(),
		"error_kind", placementErrorKind(left.Cause),
	)
}

// release frees the session's compute through api.CleanupRuntime, whose contract makes it safe on
// any backend that implements it (no dedicated per-session resources), even if another host wrote
// to the journal after settle read it.
func (g *computeGuard) release(ctx context.Context) error {
	cleanup, ok := g.p.backend.(api.CleanupRuntime)
	if !ok {
		return errors.New("the backend does not implement CleanupRuntime")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), releaseTimeout)
	defer cancel()
	_, finish := observability.BeginDebug(ctx, g.p.logger, "placement", "release_compute",
		"session_uid", g.sessionUID,
		"incarnation_id", g.inc.ID,
	)
	err := cleanup.ReleaseSession(ctx, g.sessionUID)
	finish(err, "error_kind", placementErrorKind(err))
	if err != nil {
		return &fixedTextError{msg: "release failed", err: err}
	}
	return nil
}

// fixedTextError is a Cleanup outcome whose message is the fixed text msg, since the client sees
// it, while err, a journal or backend error, stays reachable through errors.Is and errors.As. The
// release's own debug event records err's error_kind.
type fixedTextError struct {
	msg string
	err error
}

func (e *fixedTextError) Error() string { return e.msg }

func (e *fixedTextError) Unwrap() error { return e.err }

// ownership is what a journal says about compute this Placer's backend returned.
type ownership int

const (
	// unowned: the journal's compute state (ownershipEvent) is not LIVE, or LIVE is other compute.
	unowned ownership = iota
	// owned: LIVE is this compute.
	owned
	// ownerUnknown: LIVE, and the journal does not say whether it ran on this Placer: after a resume
	// that may have run on this Placer or on another one, or in a journal that does not attribute
	// every turn to one harness (executionHarnesses).
	ownerUnknown
)

// ownership reports whether recs leave the session's compute LIVE, by the guard's own fold
// (ownershipState, which unlike the stored SES-8 compute_state skips a CloseApproval receipt), on
// compute this Placer's backend returned:
//
//   - LIVE must come from this Placer (liveOwner, turnRanHere). After another Placer's turn or
//     resume (an ExecRequest.harness override, either way), LIVE is that Placer's compute.
//   - The returned instance must not have replaced the one this harness's latest turn, or a fork
//     child's FORK marker, recorded (controller.LatestComputeInstance, attributed by ranHere as the
//     controller's own check is). The controller refuses such a replacement with
//     api.ErrMemoryStateLost before it appends anything, and LIVE is the replaced instance.
//
// When the journal does not attribute every turn to one harness (executionHarnesses: a journal the
// controller rejects, though its chain verifies), or LIVE comes from a resume that may have run on
// this Placer or on another one (liveOwner names harnesses on both sides), it is ownerUnknown, on a
// standalone Placer too: releasing could take compute the journal owns, and keeping it could leak
// it.
func (g *computeGuard) ownership(recs []eventlog.Record) ownership {
	if ownershipState(recs) != api.ComputeLive {
		return unowned
	}
	if g.inc.ComputeInstance != "" {
		if last, _ := controller.LatestComputeInstance(recs, g.p.ranHere); last != "" && last != g.inc.ComputeInstance {
			return unowned
		}
	}
	harnesses, known := executionHarnesses(recs)
	if !known {
		return ownerUnknown
	}
	recorded, others := liveOwner(recs, harnesses)
	here := g.turnRanHere(recorded)
	for name := range others {
		if g.turnRanHere(name) != here {
			return ownerUnknown
		}
	}
	if here {
		return owned
	}
	return unowned
}

// ownershipEvent is computestate.Event for deciding who owns compute, except that a CloseApproval
// receipt (a TOOL_RESULT closing an approval gate with NOT_EXECUTED or OUTCOME_UNKNOWN) moves
// nothing. The host writes that receipt without a harness, an executor or compute (APR-18): the
// controller rewrites either code from an executor to EXECUTOR_ERROR and harnesswire refuses it
// from a harness, so in a verified journal it proves no compute ran. SES-8 still counts it as
// LIVE, but after a park's SUSPEND it would make compute a later Resume or Exec allocated look
// owned by the closed turn, and a failed operation would neither release nor report it.
func ownershipEvent(ev api.Event) (api.ComputeState, bool) {
	r := ev.Result
	if ev.Kind == api.EventToolResult && r != nil && r.IsError && r.ApprovalDecisionSeq > 0 &&
		(r.Code == api.ToolResultCodeNotExecuted || r.Code == api.ToolResultCodeOutcomeUnknown) {
		return api.ComputeNone, false
	}
	return computestate.Event(ev)
}

// ownershipState folds recs from NONE as computestate.Of does, through ownershipEvent.
func ownershipState(recs []eventlog.Record) api.ComputeState {
	state := api.ComputeNone
	for _, r := range recs {
		if s, moves := ownershipEvent(r.Event); moves {
			state = s
		}
	}
	return state
}

// turnRanHere reports whether a turn recorded under the harness name recorded ran on this Placer.
// Unlike ranHere, which counts every turn it cannot attribute because its callers can only refuse
// on an overcount, it counts only turns it can: a wrong yes here leaves compute neither released
// nor reported. A standalone Placer has one backend, so every turn ran on it. In a Registry, an
// empty name is a turn on the session's own harness (api.ExecutionStart.Harness), which is this
// Placer unless the operation is an override; any other name counts when it resolves to this
// Placer, so an alias does and a name this host does not serve does not.
func (g *computeGuard) turnRanHere(recorded string) bool {
	if g.p.registry == nil {
		return true
	}
	if recorded == "" {
		return !g.override
	}
	q, ok := g.p.registry.lookup(recorded)
	return ok && q == g.p
}

// liveOwner returns the harness name recorded for whatever left recs LIVE (ownershipEvent), read
// through turnHarness ("" is the session's own harness), and the names of the other harnesses that
// may have run it instead, which the journal cannot rule out (none for a turn record). harnesses is
// executionHarnesses of the whole of recs, which must attribute every turn (known): then every
// record's ExecutionID has its EXECUTION_START, if any, at or before the record, so the records
// after the one that decides change nothing.
//
// A turn record is attributed to the turn its ExecutionID names (turnHarness), not to the latest
// EXECUTION_START before it: executions can interleave (controller.ResumeInvocation selects the
// last one first seen, with an earlier one's records after it), and a fork child that re-drove an
// inherited turn writes that turn's records after its FORK marker, under the START its parent
// wrote. A LIFECYCLE_RESUME is placement's, appended after controller.Resume, and the journal does
// not record the harness it ran on. Registry.Resume picks it before any fence: the recorded harness
// of the pending turn, or the session's own harness with nothing pending. A Resume that finds a
// pending turn re-drives it and appends at least its END first, but a Resume whose pick went stale
// runs anyway: two Resume calls without expected_last_seq on hosts sharing the journal can both
// pick harness B for a turn pending on B; the first appends END and RESUME, and the second finds
// nothing pending (controller.Resume does not check the harness then), restores on B and appends a
// second RESUME, after which any record (a SUSPEND, an approval decision, a later turn on another
// harness) may come before it. So a RESUME ran on the session's own harness or on the harness of
// any turn that was ever pending in this journal: every turn with a record after the latest FORK
// marker before the RESUME, or every turn when there is none. A fork child's journal starts at its
// FORK marker; the turns before it completed in the parent, except an inherited one that the child
// re-drove, which then has a record after the marker.
func liveOwner(recs []eventlog.Record, harnesses map[string]string) (recorded string, others iter.Seq[string]) {
	for i := len(recs) - 1; i >= 0; i-- {
		ev := recs[i].Event
		if _, moves := ownershipEvent(ev); !moves {
			continue
		}
		if ev.Kind != api.EventLifecycle {
			return turnHarness(harnesses, ev), noHarnesses
		}
		return "", resumeCandidates(recs[:i], harnesses)
	}
	return "", noHarnesses
}

func noHarnesses(func(string) bool) {}

// resumeCandidates yields the recorded harness name (turnHarness, through harnesses as liveOwner
// passes it) of every turn with a record in recs after the latest FORK marker, or anywhere in recs
// when there is none: the turns a RESUME that follows recs may have re-driven or picked. It yields
// each name once.
func resumeCandidates(recs []eventlog.Record, harnesses map[string]string) iter.Seq[string] {
	return func(yield func(string) bool) {
		start := 0
		for i := len(recs) - 1; i >= 0; i-- {
			if ev := recs[i].Event; ev.Kind == api.EventLifecycle && ev.Lifecycle != nil && ev.Lifecycle.Kind == api.LifecycleFork {
				start = i + 1
				break
			}
		}
		// harnesses covers the whole journal, so an inherited turn re-driven after the marker
		// resolves to the START its parent wrote before it.
		var yielded []string
		for _, r := range recs[start:] {
			if !isTurnRecord(r.Event) {
				continue
			}
			name := turnHarness(harnesses, r.Event)
			if slices.Contains(yielded, name) {
				continue
			}
			if !yield(name) {
				return
			}
			yielded = append(yielded, name)
		}
	}
}

// isTurnRecord reports whether ev is a record a turn wrote: one that implies LIVE compute
// (computestate.Event) other than a lifecycle marker. It counts a CloseApproval receipt, which
// ownershipEvent does not: here a record can only add a candidate harness, which moves the guard
// toward reporting, never toward keeping compute as the journal's.
func isTurnRecord(ev api.Event) bool {
	_, moves := computestate.Event(ev)
	return moves && ev.Kind != api.EventLifecycle
}

// executionHarnesses maps each ExecutionID in recs to the harness name its EXECUTION_START
// recorded; an ExecutionID with no START (an older writer's turn) is absent. known reports whether
// the journal attributes every turn record to one harness. It does not when recs hold a record the
// controller rejects for who wrote it (controller recordedExecutions and legacyPrefix), though the
// hash chain verifies: an EXECUTION_START with no ExecutionID or no payload, one whose ExecutionID
// has records before it but no START (late), or one naming a harness other than its ExecutionID's
// first START (conflicting); a record with no ExecutionID other than a lifecycle marker after the
// first modern record (a START or a record with an ExecutionID), or, before it, one of a kind no
// legacy writer (v0.1.2) wrote; or a first modern record that is not a START after an unfinished
// legacy turn (its last record, lifecycle markers aside, is not END or ERROR), which may continue
// that turn (controller legacyBoundaryComplete). A repeated START naming the same harness keeps
// known: the controller rejects it too, but it names the turn's harness either way. Names are
// compared as recorded, so a START naming an alias of the first START's harness counts as
// conflicting. With harnesses incomplete, the guard reports rather than keeping or releasing
// compute on a guess.
func executionHarnesses(recs []eventlog.Record) (harnesses map[string]string, known bool) {
	harnesses = make(map[string]string)
	seen := make(map[string]bool) // ExecutionIDs with a record other than a START so far
	modern := false
	var legacyLast api.EventKind // kind of the last legacy record so far; "" before any
	for _, r := range recs {
		ev := r.Event
		if ev.Kind == api.EventLifecycle {
			continue
		}
		if ev.Kind == api.EventExecutionStart {
			modern = true
			if ev.ExecutionID == "" || ev.ExecutionStart == nil {
				return harnesses, false
			}
			name, started := harnesses[ev.ExecutionID]
			switch {
			case !started && seen[ev.ExecutionID]:
				return harnesses, false
			case started && name != ev.ExecutionStart.Harness:
				return harnesses, false
			}
			harnesses[ev.ExecutionID] = ev.ExecutionStart.Harness
			continue
		}
		if ev.ExecutionID != "" {
			if !modern && legacyLast != "" && legacyLast != api.EventEnd && legacyLast != api.EventError {
				return harnesses, false
			}
			modern = true
			seen[ev.ExecutionID] = true
			continue
		}
		if modern || !legacyKind(ev.Kind) {
			return harnesses, false
		}
		legacyLast = ev.Kind
	}
	return harnesses, true
}

// legacyKind reports whether a v0.1.2 writer recorded records of kind k, all with no ExecutionID
// (controller legacyPrefix).
func legacyKind(k api.EventKind) bool {
	switch k {
	case api.EventInput, api.EventModelCall, api.EventOutput, api.EventToolCall,
		api.EventToolResult, api.EventUsage, api.EventEnd, api.EventError:
		return true
	}
	return false
}

// turnHarness returns the harness name recorded on the EXECUTION_START of the turn that wrote ev,
// an EXECUTION_START included, found by ev's ExecutionID in harnesses (executionHarnesses). A
// legacy record (no ExecutionID) and one whose ExecutionID has no EXECUTION_START (an older
// writer's turn) return "": both ran on the session's own harness, as a session with no recorded
// turn does (Registry.Resume runs one with no pending turn there too).
func turnHarness(harnesses map[string]string, ev api.Event) string {
	if ev.ExecutionID == "" {
		return ""
	}
	return harnesses[ev.ExecutionID]
}
