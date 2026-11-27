package session_test

import (
	"context"
	"fmt"
	"sync"

	"github.com/agent-primitives/sessions/runtime/substrate"
)

// actorControl is an in-memory substrate control plane for the substrate runtime: actors with a
// status and the snapshot they hold, a new snapshot on every suspend of a running actor, and tags
// that clone them.
// onResume and onGet, when set, run before ResumeActor and GetActor answer, outside the lock.
type actorControl struct {
	mu        sync.Mutex
	status    map[string]substrate.ActorStatus
	snapshots map[string]string
	suspends  int
	onResume  func(actor string)
	onSuspend func(actor string)
	onGet     func(actor string)
	// getErr and suspendErr, when set and returning an error, fail GetActor (after onGet ran) and
	// SuspendActor with it, as a racing delete or another operation holding the actor's lease does.
	getErr     func(actor string) error
	suspendErr func(actor string) error
	// snapPrefix, when set, starts the name of every snapshot SuspendActor captures.
	snapPrefix string
	// onTag, when set, runs when TagActor tags actor; tagErr, when set, is what TagActor then returns.
	onTag  func(actor string)
	tagErr error
	// onCreate, when set, runs before CreateActor looks at the registry.
	onCreate func(actor string)
	// resumeErr, when set and returning an error, fails ResumeActor with it after onResume ran,
	// whether or not the actor exists, as substrate does for an actor whose template volume is gone.
	resumeErr func(actor string) error
	// uids is each actor's server-assigned UID, minted at create and dropped at delete, so an actor
	// created again under a name reports a new one.
	uids    map[string]string
	created int
	// captures counts each actor's captures, for CaptureLineage and NoCapture.
	captures map[string]int
}

// lineage is the actor's CaptureLineage: the capture its next suspend would take, or the one it
// holds when that suspend is a no-op. c.mu must be held.
func (c *actorControl) lineage(name string) string {
	n := c.captures[name]
	if c.status[name] != substrate.StatusSuspended || c.snapshots[name] == "" {
		n++
	}
	return fmt.Sprintf("capture-%d", n)
}

// newUID mints the UID substrate assigns an actor at creation. c.mu must be held.
func (c *actorControl) newUID(name string) {
	if c.uids == nil {
		c.uids = map[string]string{}
	}
	c.created++
	c.uids[name] = fmt.Sprintf("uid-%d", c.created)
}

func (c *actorControl) CreateActor(_ context.Context, a substrate.ActorRef, _ substrate.ObjectRef) (substrate.ActorInfo, error) {
	if c.onCreate != nil {
		c.onCreate(a.Name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.status[a.Name]; ok {
		return substrate.ActorInfo{}, fmt.Errorf("%w: %q", substrate.ErrActorExists, a.Name)
	}
	c.status[a.Name] = substrate.StatusSuspended
	c.newUID(a.Name)
	return substrate.ActorInfo{}, nil
}

func (c *actorControl) ResumeActor(_ context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	if c.onResume != nil {
		c.onResume(a.Name)
	}
	if c.resumeErr != nil {
		if err := c.resumeErr(a.Name); err != nil {
			return substrate.ActorInfo{}, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.status[a.Name]; !ok {
		return substrate.ActorInfo{}, substrate.ErrActorNotFound
	}
	c.status[a.Name] = substrate.StatusRunning
	// The actor as the resume left it, as GetActor reports it: the snapshot it restored from stays
	// its newest until its next capture.
	fidelity := substrate.SnapshotFidelityMemory
	if c.snapshots[a.Name] == "" {
		fidelity = substrate.SnapshotFidelityUnknown
	}
	return substrate.ActorInfo{Status: substrate.StatusRunning, Worker: "w", Snapshot: c.snapshots[a.Name], SnapshotFidelity: fidelity, UID: c.uids[a.Name], CaptureLineage: c.lineage(a.Name), NoCapture: c.captures[a.Name] == 0}, nil
}

func (c *actorControl) SuspendActor(_ context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	if c.suspendErr != nil {
		if err := c.suspendErr(a.Name); err != nil {
			return substrate.ActorInfo{}, err
		}
	}
	c.mu.Lock()
	if _, ok := c.status[a.Name]; !ok {
		c.mu.Unlock()
		return substrate.ActorInfo{}, substrate.ErrActorNotFound
	}
	// Substrate's suspend is a no-op on an actor that is already SUSPENDED holding a snapshot.
	snap := c.snapshots[a.Name]
	if c.status[a.Name] != substrate.StatusSuspended || snap == "" {
		if c.captures == nil {
			c.captures = map[string]int{}
		}
		c.captures[a.Name]++
		c.suspends++
		snap = fmt.Sprintf("%ssnap-%s-%d", c.snapPrefix, a.Name, c.suspends)
		c.status[a.Name], c.snapshots[a.Name] = substrate.StatusSuspended, snap
	}
	uid, lineage, noCapture := c.uids[a.Name], c.lineage(a.Name), c.captures[a.Name] == 0
	c.mu.Unlock()
	if c.onSuspend != nil {
		c.onSuspend(a.Name)
	}
	return substrate.ActorInfo{Status: substrate.StatusSuspended, Worker: "w", Snapshot: snap, SnapshotFidelity: substrate.SnapshotFidelityMemory, UID: uid, CaptureLineage: lineage, NoCapture: noCapture}, nil
}

func (c *actorControl) DeleteActor(_ context.Context, a substrate.ActorRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.status, a.Name)
	delete(c.uids, a.Name)
	// The snapshot went with the actor: one created again under the name holds none.
	delete(c.snapshots, a.Name)
	delete(c.captures, a.Name)
	return nil
}

func (c *actorControl) GetActor(_ context.Context, a substrate.ActorRef) (substrate.ActorInfo, error) {
	if c.onGet != nil {
		c.onGet(a.Name)
	}
	if c.getErr != nil {
		if err := c.getErr(a.Name); err != nil {
			return substrate.ActorInfo{}, err
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	st, ok := c.status[a.Name]
	if !ok {
		return substrate.ActorInfo{}, substrate.ErrActorNotFound
	}
	fidelity := substrate.SnapshotFidelityMemory
	noHistory := c.snapshots[a.Name] == ""
	if noHistory {
		fidelity = substrate.SnapshotFidelityUnknown
	}
	return substrate.ActorInfo{Status: st, Worker: "w", Snapshot: c.snapshots[a.Name], SnapshotFidelity: fidelity, NoSnapshotHistory: noHistory, UID: c.uids[a.Name], CaptureLineage: c.lineage(a.Name), NoCapture: c.captures[a.Name] == 0}, nil
}

func (c *actorControl) TagActor(_ context.Context, a substrate.ActorRef, _ substrate.SnapshotID) (substrate.TagInfo, error) {
	if c.onTag != nil {
		c.onTag(a.Name)
	}
	return substrate.TagInfo{}, c.tagErr
}

func (c *actorControl) CreateActorFromTag(_ context.Context, a substrate.ActorRef, _ substrate.ObjectRef, tag substrate.SnapshotID) (substrate.ActorInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status[a.Name] = substrate.StatusSuspended
	c.newUID(a.Name)
	// The clone borrows the tag's snapshot until its own first capture, as substrate's does.
	snap := "snap-" + tag.Name
	c.snapshots[a.Name] = snap
	return substrate.ActorInfo{Status: substrate.StatusSuspended, Snapshot: snap, SnapshotFidelity: substrate.SnapshotFidelityMemory, UID: c.uids[a.Name]}, nil
}

func (c *actorControl) DeleteTag(context.Context, substrate.SnapshotID) error { return nil }
