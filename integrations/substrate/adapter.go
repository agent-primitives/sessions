// Package ateadapter implements the frozen agentsessions runtime/substrate.ControlClient over the
// REAL agent-substrate ate-api Control gRPC (go.ate.dev/substrate/pkg/proto/ateapipb).
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
	atepb "go.ate.dev/substrate/pkg/proto/ateapipb"
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
// The suspend is synchronous. Substrate v0.6.0 defaults SuspendOptions.mode to
// DURABILITY_MODE_ASYNCHRONOUS, which returns once the snapshot is captured on the node, so the
// request asks for DURABILITY_MODE_SYNCHRONOUS: SuspendActor in
// cmd/ateapi/internal/controlapi/workflow_suspend.go then checkpoints a running actor with
// UPLOAD_MODE_IMMEDIATE and marks the newest generation's durable copy COMPLETED before the call
// returns, the point substrate v0.5.0 acknowledged at too. The adapter does not rely on that: an
// actor that is already SUSPENDED is returned with the snapshot it holds in any mode, and a suspend
// the worker requested (cmd/ateapi/internal/workerservice/suspend.go) runs asynchronously, so
// actorInfo reports a newest generation that is not COMPLETED as SnapshotNotReady, which the
// backend refuses rather than return or restore a checkpoint.
func (ad *Adapter) SuspendActor(ctx context.Context, actor substrate.ActorRef) (substrate.ActorInfo, error) {
	resp, err := ad.ctl.SuspendActor(ctx, &atepb.SuspendActorRequest{
		Actor:   objectRef(actor),
		Options: &atepb.SuspendOptions{Mode: atepb.DurabilityMode_DURABILITY_MODE_SYNCHRONOUS},
	})
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
// worker assignment whenever the actor holds no worker (SUSPENDED and CRASHED).
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
// process memory of the snapshot ResumeActor restores from. That is substrate's own choice, not
// heldSnapshot's, and this restates it from substrate v0.6.0
// cmd/ateapi/internal/controlapi/workflow_resume.go: the source is snapshotToResume, the latest
// snapshot that has a node-local copy or a COMPLETED durable copy (a tag borrowed at creation
// included), and requiredSnapshotRuntime makes its recorded runtime a requirement only for a MEMORY
// snapshot. When no worker matching that runtime has room, ResumeActor may drop the requirement
// (canRelaxRuntime), binds another worker, restores the snapshot at ROOTFS scope (runtimeIncompatible
// in the same file) and still returns the actor RUNNING with no error. A resume that may not drop it
// (a node-pinned actor, or a source that is local only) is bound to a matching worker, so comparing
// the snapshot's runtime with the assignment's (WorkerAssignment.sandbox_runtime) on the response
// is the only evidence the client gets, and it holds in either case. The assignment is not relabeled
// while the actor runs, and every snapshot taken on it is stamped with its runtime, so the evidence
// holds until the next capture.
//
// A snapshot with no recorded runtime imposes no constraint, so it is never relaxed. A tag-owned
// snapshot on an actor created without a source tag is the template's golden snapshot (CreateActor
// in cmd/ateapi/internal/controlapi/actor.go), which holds no session state; a fork clone keeps its
// source_tag, so its borrowed snapshot counts. Substrate also restores at VOLUMES scope when the
// snapshot was captured under an older ActorTemplate UID (templateReplaced); the response does not
// carry the template's UID, so that case is not reported here (sessions issue #192).
func memoryNotRestored(a *atepb.Actor) bool {
	st := a.GetStatus()
	assignment := st.GetWorkerAssignment()
	if assignment == nil {
		return false
	}
	var src *atepb.Snapshot
	for _, snap := range st.GetSnapshots() {
		restorable := snap.GetLocality() != "" ||
			snap.GetDurableSnapshot().GetStatus() == atepb.SnapshotStorageStatus_SNAPSHOT_STORAGE_STATUS_COMPLETED
		if restorable && (src == nil || snap.GetGeneration() >= src.GetGeneration()) {
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
// the snapshot a SuspendActor of the actor ends up holding, as substrate v0.6.0 assigns them
// (cmd/ateapi/internal/controlapi/workflow_suspend.go):
//
//   - SUSPENDING: the capture in progress, the newest generation. ensureMarkedSuspending assigned
//     it and stamped it with the worker's runtime, and a SuspendActor that re-enters the workflow
//     completes that one.
//   - Any other state with a worker assignment (RUNNING, or RESUMING once bound): the next capture.
//     ensureMarkedSuspending gives it last_assigned_generation+1 and stamps it with a copy of
//     WorkerAssignment.sandbox_runtime, and the assignment cannot change in place.
//   - No assignment (SUSPENDED, or CRASHED): the newest generation, which a SuspendActor of a
//     SUSPENDED actor returns as it is, in any mode.
//
// So the SuspendActor response reports the value an earlier GetActor did only if the suspend
// captured the state that read saw. A capture in between followed by a resume moves the next
// capture to a newer generation, and a resume onto a worker of another runtime changes the stamp.
// A single capture with no resume after it takes the predicted generation and
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

// noCapture is ActorInfo.NoCapture: the actor holds no snapshot captured from it. Substrate v0.6.0
// creates an actor with no snapshot, or with the one snapshot of its source tag (a fork's, or the
// template's golden tag) as generation 1, owned by the tag (CreateActor in
// cmd/ateapi/internal/controlapi/actor.go). Every suspend of the actor, synchronous or not, assigns
// a new generation owned by the actor (ensureMarkedSuspending in workflow_suspend.go). So the newest snapshot owned by a tag at the last assigned generation, or
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
// a snapshot stamped with recorded. It restates Requirement.Matches in substrate v0.6.0
// internal/hardware/hardware.go, which ResumeActor uses for that decision and which is not
// importable: the sandbox classes are equal, the compatibility schema versions are equal and
// nonempty, and every attribute recorded on the snapshot is present on assigned and satisfied by
// it. cpu_features is satisfied when the snapshot's feature set is a subset of assigned's, and a
// value that is not the canonical encoding (cpuFeatureMask) on either side satisfies nothing; every
// other attribute must be equal. Attributes only assigned has are ignored. A missing assigned
// runtime cannot restore it.
func runtimeRestores(assigned, recorded *atepb.ActorSandboxRuntime) bool {
	if assigned == nil || assigned.GetSandboxClass() != recorded.GetSandboxClass() {
		return false
	}
	have, want := assigned.GetVersion(), recorded.GetVersion()
	if have == nil || want == nil || have.GetSchemaVersion() == "" ||
		have.GetSchemaVersion() != want.GetSchemaVersion() {
		return false
	}
	for _, attr := range want.GetAttributes() {
		i := slices.IndexFunc(have.GetAttributes(), func(got *atepb.AttributeEntry) bool {
			return got.GetKey() == attr.GetKey()
		})
		if i < 0 || !attributeSatisfied(attr.GetKey(), have.GetAttributes()[i].GetValue(), attr.GetValue()) {
			return false
		}
	}
	return true
}

// cpuFeaturesAttr is the attribute key substrate v0.6.0 records a gVisor runtime's CPU feature set
// under (hardware.AttrCPUFeatures).
const cpuFeaturesAttr = "cpu_features"

// attributeSatisfied reports whether a worker's value for key satisfies the snapshot's, as
// Requirement.attributeSatisfied in substrate v0.6.0 internal/hardware/hardware.go does.
func attributeSatisfied(key, have, want string) bool {
	if key != cpuFeaturesAttr {
		return have == want
	}
	w, ok := cpuFeatureMask(want)
	if !ok {
		return false
	}
	h, ok := cpuFeatureMask(have)
	if !ok {
		return false
	}
	for i := range w {
		if w[i]&^h[i] != 0 {
			return false
		}
	}
	return true
}

// cpuFeatureMaskWords is the width of a cpu_features bitmask in 64-bit words (featureMaskWords in
// substrate v0.6.0 internal/hardware/cpufeatures.go).
const cpuFeatureMaskWords = 6

// cpuFeatureMask parses a cpu_features value in its canonical encoding, as parseFeatureMask in
// substrate v0.6.0 internal/hardware/cpufeatures.go does: lowercase hex with no prefix and no
// leading zero ("0" for the empty set), at most cpuFeatureMaskWords words. Anything else is
// refused, so a non-canonical mask matches nothing.
func cpuFeatureMask(s string) ([cpuFeatureMaskWords]uint64, bool) {
	var m [cpuFeatureMaskWords]uint64
	if s == "" || len(s) > cpuFeatureMaskWords*16 || (len(s) > 1 && s[0] == '0') {
		return m, false
	}
	for i := 0; i < len(s); i++ {
		c := s[len(s)-1-i]
		var v uint64
		switch {
		case '0' <= c && c <= '9':
			v = uint64(c - '0')
		case 'a' <= c && c <= 'f':
			v = uint64(c-'a') + 10
		default:
			return [cpuFeatureMaskWords]uint64{}, false
		}
		m[i/16] |= v << (4 * (i % 16))
	}
	return m, true
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
// Substrate v0.6.0 has no pause: an asynchronous suspend (one the worker requested, or a client
// that leaves SuspendOptions.mode unset) leaves the actor SUSPENDED with a node-local newest
// generation whose durable copy is PENDING until atelet's background upload completes
// (ensureSuspendedFinalized in cmd/ateapi/internal/controlapi/workflow_suspend.go). It maps to
// StatusSuspended. What tells it apart is its snapshots, so actorInfo reports it as
// SnapshotNotReady and SnapshotPending, and the backend refuses to restore or attach to it, as a
// retryable failure, until the upload completes. A local-only or unrecognized durable status is
// SnapshotNotReady without SnapshotPending and is refused. v0.6.0 never writes one (every suspend
// creates its generation PENDING), and suspending again does not upload it: SuspendActor returns a
// SUSPENDED actor as it is in any mode.
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
