// Package ateadapter implements the frozen agentsessions runtime/substrate.ControlClient over the
// REAL agent-substrate ate-api Control gRPC (github.com/agent-substrate/substrate/pkg/proto/ateapipb).
//
// Neutrality (hard rule): this lives in its OWN Go module so the core agentsessions module imports
// zero substrate code and its go.mod stays clean. runtime/substrate.ControlClient is the seam —
// substrate is one backend that adapts to our interface, not a dependency of the neutral core.
//
// Mapping: Create->CreateActor; Resume->ResumeActor; Suspend->SuspendActor; Delete->DeleteActor;
// Status->GetActor; clone->CreateTag{source_actor}+CreateActor{source_tag}; release->DeleteTag. The
// interface is frozen our side; this is a pure translation of the generated client.
package ateadapter

import (
	"context"
	"encoding/base64"
	"fmt"
	"slices"

	"github.com/agent-primitives/sessions/runtime/substrate"
	atepb "github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// Adapter implements substrate.ControlClient over the ate-api Control gRPC client.
type Adapter struct {
	ctl atepb.ControlClient
}

var _ substrate.ControlClient = (*Adapter)(nil)
var _ substrate.SnapshotCloner = (*Adapter)(nil)

// New wraps an ate-api Control client over conn. The caller owns dialing and authentication.
func New(conn grpc.ClientConnInterface) *Adapter {
	return &Adapter{ctl: atepb.NewControlClient(conn)}
}

// FromClient wraps an already-constructed ate-api Control client (used in tests with a double).
func FromClient(ctl atepb.ControlClient) *Adapter {
	return &Adapter{ctl: ctl}
}

func objectRef(a substrate.ActorRef) *atepb.ObjectRef {
	return &atepb.ObjectRef{Atespace: a.Atespace, Name: a.Name}
}

func templateRef(t substrate.ObjectRef) *atepb.ObjectRef {
	return &atepb.ObjectRef{Atespace: t.Atespace, Name: t.Name}
}

func tagRef(t substrate.SnapshotID) *atepb.ObjectRef {
	return &atepb.ObjectRef{Atespace: t.Atespace, Name: t.Name}
}

// CreateActor creates an actor from an ActorTemplate (atespace + name). With no source tag, substrate
// seeds it from the template's golden snapshot when one is built. A name already taken is reported as
// substrate.ErrActorExists, so the backend can attach to the actor another host just created.
func (ad *Adapter) CreateActor(ctx context.Context, actor substrate.ActorRef, template substrate.ObjectRef) error {
	_, err := ad.ctl.CreateActor(ctx, &atepb.CreateActorRequest{
		Actor: &atepb.Actor{
			Metadata:      &atepb.ResourceMetadata{Atespace: actor.Atespace, Name: actor.Name},
			ActorTemplate: templateRef(template),
		},
	})
	if status.Code(err) == codes.AlreadyExists {
		return fmt.Errorf("%w: %s/%s: %w", substrate.ErrActorExists, actor.Atespace, actor.Name, err)
	}
	return err
}

// ResumeActor schedules the actor onto a worker. Substrate restores the actor's own external
// snapshot when it holds one (including a tag's, borrowed at creation) and cold-boots the template
// spec otherwise; the request carries no boot flag. Substrate refuses a resume while another
// operation holds the actor's lease with ABORTED, reported as substrate.ErrActorBusy, and a missing
// actor with NOT_FOUND (RPCService.ResumeActor in cmd/ateapi/internal/controlapi/actor.go), reported
// as substrate.ErrActorNotFound. Substrate can also answer NOT_FOUND for an actor that exists, when
// a volume is missing from its template (createActorVolumes in
// cmd/ateapi/internal/controlapi/volumes.go, called by ensureVolumesCreated in workflow_resume.go),
// so the backend checks the actor with GetActor before it reads either as the actor being gone.
func (ad *Adapter) ResumeActor(ctx context.Context, actor substrate.ActorRef) (substrate.ActorInfo, error) {
	resp, err := ad.ctl.ResumeActor(ctx, &atepb.ResumeActorRequest{Actor: objectRef(actor)})
	if status.Code(err) == codes.NotFound {
		return substrate.ActorInfo{}, fmt.Errorf("%w: %s/%s: %w", substrate.ErrActorNotFound, actor.Atespace, actor.Name, err)
	}
	if err != nil {
		return substrate.ActorInfo{}, busy(actor, err)
	}
	return actorInfo(resp.GetActor()), nil
}

// SuspendActor snapshots the actor to durable storage and frees the worker, returning the actor as
// the suspend left it: the external snapshot's URI and observed fidelity (see heldSnapshot), and
// its UID. Substrate refuses a suspend while another operation holds the actor's lease with ABORTED,
// reported as substrate.ErrActorBusy, and a missing actor with NOT_FOUND (RPCService.SuspendActor in
// cmd/ateapi/internal/controlapi/actor.go maps the store's ErrNotFound from
// ActorWorkflow.SuspendActor in cmd/ateapi/internal/controlapi/workflow_suspend.go), reported as
// substrate.ErrActorNotFound, which the ControlClient contract requires: a memory-tier Snapshot
// reads it as the session's in-RAM state being gone, not as a transport failure.
//
// The suspend is synchronous. SuspendActorRequest at substrate v0.5.0 has no durability option:
// SuspendActor in cmd/ateapi/internal/controlapi/workflow_suspend.go checkpoints a running actor
// with UPLOAD_MODE_IMMEDIATE (or uploads a paused actor's local snapshot with PromoteSnapshot), and
// ensureSuspendedFinalized marks the newest generation COMPLETED before the call returns. The
// adapter does not rely on that: actorInfo reports a newest generation that is not COMPLETED as
// SnapshotNotReady, which the backend refuses rather than return or restore a checkpoint.
// Substrate after v0.5.0 adds SuspendActorRequest.options.mode and defaults it to asynchronous, so
// TestSuspendActorRequestHasNoDurabilityOption fails on a bump that adds any request field until
// this call asks for a synchronous suspend explicitly.
func (ad *Adapter) SuspendActor(ctx context.Context, actor substrate.ActorRef) (substrate.ActorInfo, error) {
	resp, err := ad.ctl.SuspendActor(ctx, &atepb.SuspendActorRequest{Actor: objectRef(actor)})
	if status.Code(err) == codes.NotFound {
		return substrate.ActorInfo{}, fmt.Errorf("%w: %s/%s: %w", substrate.ErrActorNotFound, actor.Atespace, actor.Name, err)
	}
	if err != nil {
		return substrate.ActorInfo{}, busy(actor, err)
	}
	return actorInfo(resp.GetActor()), nil
}

// busy wraps err in substrate.ErrActorBusy when substrate reported ABORTED, which it does when
// another operation holds the actor's lease (acquireLease in substrate's
// cmd/ateapi/internal/controlapi/workflow.go) or a concurrent update conflicted. Both settle on
// their own, so the caller can retry.
func busy(actor substrate.ActorRef, err error) error {
	if status.Code(err) == codes.Aborted {
		return fmt.Errorf("%w: %s/%s: %w", substrate.ErrActorBusy, actor.Atespace, actor.Name, err)
	}
	return err
}

// DeleteActor deletes a suspended actor.
func (ad *Adapter) DeleteActor(ctx context.Context, actor substrate.ActorRef) error {
	_, err := ad.ctl.DeleteActor(ctx, &atepb.DeleteActorRequest{Actor: objectRef(actor)})
	return err
}

// GetActor reads the actor's current state. A missing actor is reported as substrate.ErrActorNotFound
// so the backend can tell "never placed" (create it) apart from "already placed" (attach to it)
// without inspecting gRPC codes itself.
func (ad *Adapter) GetActor(ctx context.Context, actor substrate.ActorRef) (substrate.ActorInfo, error) {
	a, err := ad.ctl.GetActor(ctx, &atepb.GetActorRequest{Actor: objectRef(actor)})
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return substrate.ActorInfo{}, fmt.Errorf("%w: %s/%s", substrate.ErrActorNotFound, actor.Atespace, actor.Name)
		}
		return substrate.ActorInfo{}, err
	}
	return actorInfo(a), nil
}

// TagActor tags the external snapshot the suspended source actor holds. Substrate copies the snapshot
// into tag-owned storage inside the call, so the tag is usable as soon as it returns. The tag uses
// the ATESPACE scope: it may seed actors only in the source's atespace, which is all a fork needs
// because the child is created alongside its parent. Publishing (cross-atespace reuse) is
// deliberately not done here.
//
// A name already taken, whether by a finished tag or one a failed create left pending, is reported
// as substrate.ErrTagExists, so the backend does not delete a tag this call did not reserve.
// Substrate refuses a tag while another operation holds the source actor's lease with ABORTED
// (acquireLease in cmd/ateapi/internal/controlapi/workflow.go, called from TagActorSnapshot in
// cmd/ateapi/internal/controlapi/workflow_tag.go), reported as substrate.ErrActorBusy. A missing
// source actor is FAILED_PRECONDITION (RPCService.CreateTag in
// cmd/ateapi/internal/controlapi/tag.go), which the backend classifies by reading the actor.
func (ad *Adapter) TagActor(ctx context.Context, source substrate.ActorRef, tag substrate.SnapshotID) error {
	_, err := ad.ctl.CreateTag(ctx, &atepb.CreateTagRequest{
		Tag: &atepb.Tag{
			Metadata:    &atepb.ResourceMetadata{Atespace: tag.Atespace, Name: tag.Name},
			Scope:       atepb.TagScope_TAG_SCOPE_ATESPACE,
			SourceActor: objectRef(source),
		},
	})
	if status.Code(err) == codes.AlreadyExists {
		return fmt.Errorf("%w: %s/%s: %w", substrate.ErrTagExists, tag.Atespace, tag.Name, err)
	}
	return busy(source, err)
}

// CreateActorFromTag creates an actor seeded from the snapshot behind tag. Substrate requires the
// actor's template to be the one the snapshot was taken under. The actor is created SUSPENDED and
// borrows the tag's snapshot; the caller resumes it to restore the cloned RAM. CreateActor returns
// the actor it stored, with the UID substrate assigned (RPCService.CreateActor in
// cmd/ateapi/internal/controlapi/actor.go), so the caller can tell that clone from a later actor
// under the same name.
func (ad *Adapter) CreateActorFromTag(ctx context.Context, actor substrate.ActorRef, template substrate.ObjectRef, tag substrate.SnapshotID) (substrate.ActorInfo, error) {
	a, err := ad.ctl.CreateActor(ctx, &atepb.CreateActorRequest{
		Actor: &atepb.Actor{
			Metadata:      &atepb.ResourceMetadata{Atespace: actor.Atespace, Name: actor.Name},
			ActorTemplate: templateRef(template),
			SourceTag:     tagRef(tag),
		},
	})
	if err != nil {
		return substrate.ActorInfo{}, err
	}
	return actorInfo(a), nil
}

// DeleteTag removes a tag and collects the snapshot copy it owns.
func (ad *Adapter) DeleteTag(ctx context.Context, tag substrate.SnapshotID) error {
	_, err := ad.ctl.DeleteTag(ctx, &atepb.DeleteTagRequest{Tag: tagRef(tag)})
	return err
}

// actorInfo maps a substrate Actor onto the narrowed ActorInfo the backend needs. The worker is
// reported for diagnostics only: the backend reaches the harness through the atenet-router, which
// resolves the actor's worker on every request, so nothing here is dialed. Substrate clears the
// worker assignment whenever the actor holds no worker (SUSPENDED, paused included, and CRASHED).
//
// UID is ResourceMetadata.uid, which substrate assigns when the actor is created and keeps across
// every later update (suspend and resume included), so an actor created again under the same name
// reports a different one.
func actorInfo(a *atepb.Actor) substrate.ActorInfo {
	st := a.GetStatus()
	snap := heldSnapshot(st)
	durable := snap.GetDurableSnapshot().GetStatus()
	notReady := snap != nil && durable != atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED
	if notReady {
		snap = nil // never expose an older completed generation as the held checkpoint
	}
	return substrate.ActorInfo{
		SnapshotNotReady: notReady,
		// Only an explicit PENDING upload can complete by waiting; a local-only or unrecognized
		// durable status needs inspection and a new durable checkpoint.
		SnapshotPending: notReady && durable == atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_PENDING,
		// Unknown legacy fields (notably pre-list external_snapshot) are not proof of no history.
		NoSnapshotHistory: st != nil && a.GetMetadata().GetUid() != "" &&
			st.GetState() == atepb.ActorState_ACTOR_STATE_SUSPENDED &&
			st.GetLastAssignedGeneration() == 0 && len(st.GetSnapshots()) == 0 &&
			len(st.ProtoReflect().GetUnknown()) == 0,
		MemoryNotRestored: memoryNotRestored(a),
		CaptureLineage:    captureLineage(st),
		NoCapture:         noCapture(st),
		Status:            actorStatus(st.GetState()),
		Worker:            st.GetWorkerAssignment().GetWorkerPod(),
		Snapshot:          snap.GetDurableSnapshot().GetObject().GetSnapshotUri(),
		SnapshotFidelity:  snapshotFidelity(snap.GetFidelity()),
		UID:               a.GetMetadata().GetUid(),
	}
}

// memoryNotRestored reports that the actor holds a worker whose sandbox runtime cannot restore the
// process memory of the snapshot ResumeActor restores from. That is substrate's own choice
// (requiredSnapshotRuntime and canRelaxRuntime in
// cmd/ateapi/internal/controlapi/workflow_resume.go), not heldSnapshot's: a node-local checkpoint
// pins the resume to its node and is never relaxed, so any snapshot with a locality rules the
// fallback out; otherwise the source is the latest COMPLETED durable generation, even when a newer
// generation is still PENDING or local-only.
//
// Since substrate v0.5.0 a snapshot records the runtime it was taken under
// (Snapshot.sandbox_runtime, agent-substrate/substrate#2536) and an assignment the runtime the
// actor was started or restored under (WorkerAssignment.sandbox_runtime). When no worker matching
// the snapshot's runtime has room, ResumeActor drops that constraint (canRelaxRuntime), binds
// another worker, restores the snapshot at VOLUMES scope (runtimeIncompatible in the same file)
// and still returns the actor RUNNING with no error. Comparing the two runtimes on the response is
// the only evidence the client gets. The assignment is not relabeled while the actor runs, and
// every snapshot taken on it is stamped with its runtime, so the evidence holds until the next
// capture.
//
// Only MEMORY snapshots count: that is the state a relaxed restore drops. A snapshot with no
// recorded runtime imposes no constraint, so it is never relaxed. A tag-owned snapshot on an actor
// created without a source tag is the template's golden snapshot (CreateActor in
// cmd/ateapi/internal/controlapi/actor.go), which holds no session state; a fork clone keeps its
// source_tag, so its borrowed snapshot counts.
func memoryNotRestored(a *atepb.Actor) bool {
	st := a.GetStatus()
	pinned := slices.ContainsFunc(st.GetSnapshots(), func(s *atepb.Snapshot) bool { return s.GetLocality() != "" })
	assignment := st.GetWorkerAssignment()
	if assignment == nil || pinned {
		return false
	}
	completed := atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED
	var src *atepb.Snapshot
	for _, snap := range st.GetSnapshots() {
		if snap.GetDurableSnapshot().GetStatus() == completed && (src == nil || snap.GetGeneration() >= src.GetGeneration()) {
			src = snap
		}
	}
	recorded := src.GetSandboxRuntime()
	switch {
	case recorded == nil,
		src.GetFidelity() != atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY,
		src.GetOwner() == atepb.SnapshotOwner_SNAPSHOT_OWNER_TAG && a.GetSourceTag() == nil:
		return false
	}
	return !runtimeRestores(assignment.GetSandboxRuntime(), recorded)
}

// captureLineage is ActorInfo.CaptureLineage: the generation and the recorded sandbox runtime of
// the snapshot a SuspendActor of the actor ends up holding, as substrate v0.5.0 assigns them
// (cmd/ateapi/internal/controlapi/workflow_suspend.go):
//
//   - SUSPENDING: the capture in progress, the newest generation. ensureMarkedSuspending assigned
//     it and stamped it with the worker's runtime (or, for a paused actor, promoted the pause's
//     local snapshot), and a SuspendActor that re-enters the workflow completes that one.
//   - Any other state with a worker assignment (RUNNING, or RESUMING once bound): the next capture.
//     ensureMarkedSuspending gives it last_assigned_generation+1 and stamps it with a copy of
//     WorkerAssignment.sandbox_runtime, and the assignment cannot change in place.
//   - No assignment (SUSPENDED, paused included, or CRASHED): the newest generation, which a
//     SuspendActor leaves as it is or, for a paused actor, uploads under the same generation and
//     stamp.
//
// So the SuspendActor response reports the value an earlier GetActor did only if the suspend
// captured the state that read saw. A capture or pause in between followed by a resume moves the
// next capture to a newer generation, and a resume onto a worker of another runtime changes the
// stamp. A single capture or pause with no resume after it takes the predicted generation and
// stamp, so the suspend that follows reports the value the read did; so does a crash, revert and
// resume onto a worker of the same runtime. "" when the status is missing or the runtime cannot
// be encoded, which the backend refuses for a memory-tier Snapshot.
func captureLineage(st *atepb.ActorStatus) string {
	if st == nil {
		return ""
	}
	gen, rt := heldSnapshot(st).GetGeneration(), heldSnapshot(st).GetSandboxRuntime()
	if assignment := st.GetWorkerAssignment(); assignment != nil && st.GetState() != atepb.ActorState_ACTOR_STATE_SUSPENDING {
		gen, rt = st.GetLastAssignedGeneration()+1, assignment.GetSandboxRuntime()
	}
	stamp := "none"
	if rt != nil {
		b, err := proto.MarshalOptions{Deterministic: true}.Marshal(rt)
		if err != nil {
			return ""
		}
		stamp = base64.RawStdEncoding.EncodeToString(b)
	}
	return fmt.Sprintf("generation %d runtime %s", gen, stamp)
}

// noCapture is ActorInfo.NoCapture: the actor holds no snapshot captured from it. Substrate v0.5.0
// creates an actor with no snapshot, or with the one snapshot of its source tag (a fork's, or the
// template's golden tag) as generation 1, owned by the tag (CreateActor in
// cmd/ateapi/internal/controlapi/actor.go). Every suspend or pause of the actor assigns a new
// generation owned by the actor (ensureMarkedSuspending in workflow_suspend.go, newLocalSnapshot in
// snapshot_helpers.go). So the newest snapshot owned by a tag at the last assigned generation, or
// no snapshot and no generation assigned, means no capture. An unspecified owner, a missing status
// or fields this client does not read are not evidence, so they report false.
func noCapture(st *atepb.ActorStatus) bool {
	if st == nil || len(st.ProtoReflect().GetUnknown()) != 0 {
		return false
	}
	newest := heldSnapshot(st)
	if newest == nil {
		return st.GetLastAssignedGeneration() == 0
	}
	return newest.GetOwner() == atepb.SnapshotOwner_SNAPSHOT_OWNER_TAG &&
		newest.GetGeneration() == st.GetLastAssignedGeneration()
}

// runtimeRestores reports whether an actor running under assigned can have restored the memory of
// a snapshot stamped with recorded. It restates Matches and MatchesCompat in substrate v0.5.0
// internal/hardware/hardware.go, which ResumeActor uses for that decision and which is not
// importable: the sandbox classes are equal, the compatibility schema versions are equal and
// nonempty, and every attribute recorded on the snapshot is present on assigned with an equal
// value. Attributes only assigned has are ignored. A missing assigned runtime cannot restore it.
func runtimeRestores(assigned, recorded *atepb.SandboxRuntime) bool {
	if assigned == nil || assigned.GetSandboxClass() != recorded.GetSandboxClass() {
		return false
	}
	have, want := assigned.GetVersion(), recorded.GetVersion()
	if have == nil || want == nil || have.GetSchemaVersion() == "" ||
		have.GetSchemaVersion() != want.GetSchemaVersion() {
		return false
	}
	for _, attr := range want.GetAttributes() {
		if !slices.ContainsFunc(have.GetAttributes(), func(got *atepb.AttributeEntry) bool {
			return got.GetKey() == attr.GetKey() && got.GetValue() == attr.GetValue()
		}) {
			return false
		}
	}
	return true
}

// heldSnapshot selects the highest generation before inspecting durable readiness; later ties win.
// A newest pending or local-only checkpoint makes actorInfo report not ready with no URI/fidelity,
// rather than exposing an older completed generation. This deliberately refuses more than
// Substrate's latest-completed durable selection to avoid silently restoring stale state.
// A completed entry with no URI never falls back either. A tag-seeded actor borrows generation 1.
//
// This is not effective restore-source evidence: Substrate can prefer node-local state or downgrade
// after an ActorTemplate UID change. Those limits remain tracked separately in sessions issue #192.
//
// Substrate f03327bd replaced the single ActorStatus.external_snapshot (field 4) with this list. A
// server older than that still sends field 4, which this client does not read, so it reports no
// handle or fidelity. A memory checkpoint refuses before SUSPEND is recorded; a direct Backend.Fork
// with no handle refuses with ErrNoSnapshotToClone rather than tagging a snapshot it cannot name.
func heldSnapshot(st *atepb.ActorStatus) *atepb.Snapshot {
	var held *atepb.Snapshot
	for _, snap := range st.GetSnapshots() {
		if held == nil || snap.GetGeneration() >= held.GetGeneration() {
			held = snap
		}
	}
	return held
}

func snapshotFidelity(f atepb.SnapshotFidelity) substrate.SnapshotFidelity {
	switch f {
	case atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_MEMORY:
		return substrate.SnapshotFidelityMemory
	case atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_VOLUMES:
		return substrate.SnapshotFidelityVolumes
	case atepb.SnapshotFidelity_SNAPSHOT_FIDELITY_ROOTFS:
		return substrate.SnapshotFidelityRootFS
	default:
		return substrate.SnapshotFidelityUnknown
	}
}

// actorStatus narrows substrate's lifecycle onto the states the backend acts on. RESUMING and
// SUSPENDING map to StatusTransitioning: they settle into RUNNING or SUSPENDED on their own (often
// another host's resume), so the backend reports them as retryable. DELETING, REVERTING and any
// state this client does not know map to StatusUnknown, which the backend refuses to place onto
// rather than guess which side it will land on.
//
// Substrate v0.5.0 has no PAUSED or PAUSING state (agent-substrate/substrate#2620): a paused actor
// is SUSPENDED with a node-local newest generation and no durable copy, and a pause in progress is
// SUSPENDING (latestSnapshotIsLocalOnly and latestSnapshotIsPendingLocal in
// cmd/ateapi/internal/controlapi/snapshot_helpers.go). They map to StatusSuspended and
// StatusTransitioning, as PAUSED and PAUSING did. What tells a paused actor apart is its snapshots,
// so actorInfo reports it as SnapshotNotReady without SnapshotPending, and the backend refuses to
// restore or attach to it until a suspend uploads that generation. A memory-tier session's suspend
// is refused too (api.ErrMemoryStateLost, runtime/substrate requireRecordedCheckpoint), so that
// actor is never usable again.
func actorStatus(s atepb.ActorState) substrate.ActorStatus {
	switch s {
	case atepb.ActorState_ACTOR_STATE_RUNNING:
		return substrate.StatusRunning
	case atepb.ActorState_ACTOR_STATE_SUSPENDED:
		return substrate.StatusSuspended
	case atepb.ActorState_ACTOR_STATE_CRASHED:
		return substrate.StatusTerminated
	case atepb.ActorState_ACTOR_STATE_RESUMING, atepb.ActorState_ACTOR_STATE_SUSPENDING:
		return substrate.StatusTransitioning
	default:
		return substrate.StatusUnknown
	}
}
