package substrate_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

var key = substrate.WithCleanupKey("key")

// Create and Restore plan the session's actor; a stateful fork also plans the tag its child borrows.
// Without a cleanup key there is no plan at all.
func TestCleanupPlans(t *testing.T) {
	if plan := newBackend(&mockControl{}).CleanupResources(api.CleanupCreate, "s"); plan != nil {
		t.Fatalf("keyless plan=%+v, want none", plan)
	}
	create := newBackend(&mockControl{}, key).CleanupResources(api.CleanupCreate, "s")
	fork := newMemoryBackend(&mockControl{}, key).CleanupResources(api.CleanupFork, "child")
	if len(create) != 1 || len(fork) != 2 || fork[0].Borrower != fork[1].Reference || api.ValidateCleanupPlan(fork) != nil {
		t.Fatalf("create plan=%+v fork plan=%+v", create, fork)
	}
}

// Create claims the actor its CreateActor made, also when a later step fails, and with no routing
// fields on the partial result. It claims nothing without a key or for an actor another caller made.
func TestCreateClaimsOnlyTheActorItCreated(t *testing.T) {
	resumeErr := errors.New("resume failed")
	for _, tc := range []struct {
		name  string
		opts  []substrate.Option
		setup func(*mockControl)
		claim bool
	}{
		{name: "created", opts: []substrate.Option{key}, claim: true},
		{name: "resume fails", opts: []substrate.Option{key}, setup: func(m *mockControl) { m.resumeErr = resumeErr }, claim: true},
		{name: "keyless", setup: func(m *mockControl) {}},
		{name: "another caller created it", opts: []substrate.Option{key}, setup: func(m *mockControl) {
			m.afterGet = func(name string) { m.afterGet = nil; m.exists(name, substrate.StatusRunning) }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &mockControl{info: running("worker-a")}
			if tc.setup != nil {
				tc.setup(m)
			}
			b := newBackend(m, tc.opts...)
			inc, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "s"})
			if (m.resumeErr != nil) != errors.Is(err, resumeErr) || m.resumeErr == nil && err != nil {
				t.Fatalf("create err=%v", err)
			}
			got := inc.Cleanup.Resources()
			if !tc.claim {
				if len(got) != 0 {
					t.Fatalf("claimed %+v, want nothing", got)
				}
				return
			}
			want := []api.CleanupResource{{Reference: b.CleanupResources(api.CleanupCreate, "s")[0].Reference, UID: m.uids["s"]}}
			if !reflect.DeepEqual(got, want) || err != nil && inc.Worker != "" {
				t.Fatalf("claim=%+v worker=%q, want %+v and no routing on a partial result", got, inc.Worker, want)
			}
		})
	}
}

// Restore claims the actor only when its UID is the one the journal recorded.
func TestRestoreClaimsOnlyTheRecordedActor(t *testing.T) {
	m := &mockControl{info: running("worker-a")}
	m.exists("s", substrate.StatusRunning)
	b := newBackend(m, key)
	for recorded, claim := range map[string]bool{m.uids["s"]: true, "replaced-uid": false} {
		inc, err := b.RestoreInstance(context.Background(), api.SnapshotRef{Local: "s"}, func() (string, error) { return recorded, nil })
		if err != nil {
			t.Fatal(err)
		}
		if got := inc.Cleanup.Resources(); claim != (len(got) == 1 && got[0].UID == recorded) || !claim && len(got) != 0 {
			t.Fatalf("recorded %q: claim=%+v", recorded, got)
		}
	}
}

// A fork claims the tag and child it created, with the tag borrowed by the child. A failed fork
// returns what it created so placement records it, and keeps main's name-based teardown.
func TestForkClaimsTagAndChild(t *testing.T) {
	ref := api.SnapshotRef{Local: "parent", ExternalURI: "snap-parent-1", Memory: true}
	fork := func(m *mockCloner) (*substrate.Backend, api.Incarnation, error) {
		b := newMemoryBackend(m, key)
		inc, err := b.Fork(context.Background(), ref, api.ForkOpts{ChildSessionUID: "child"})
		return b, inc, err
	}

	m := newClonerWithSuspendedParent()
	b, inc, err := fork(m)
	if err != nil {
		t.Fatal(err)
	}
	plan := b.CleanupResources(api.CleanupFork, "child")
	want := []api.CleanupResource{
		{Reference: plan[0].Reference, UID: "uid-fork-child", Borrower: plan[1].Reference},
		{Reference: plan[1].Reference, UID: m.uids["child"]},
	}
	if got := inc.Cleanup.Resources(); !reflect.DeepEqual(got, want) {
		t.Fatalf("claim=%+v, want %+v", got, want)
	}

	m = newClonerWithSuspendedParent()
	m.cloneErr = errors.New("clone failed")
	_, inc, err = fork(m)
	if err == nil || !reflect.DeepEqual(inc.Cleanup.Resources(), want[:1]) {
		t.Fatalf("failed clone: err=%v claim=%+v, want the tag claim %+v", err, inc.Cleanup.Resources(), want[:1])
	}
	if m.calls[len(m.calls)-1] != "untag:fork-child" {
		t.Fatalf("calls=%v, want the tag still released by name", m.calls)
	}

	// A stateless fork is a Create; when it fails, its claim is returned, not dropped.
	sm := &mockControl{info: running("worker-a"), resumeErr: errors.New("resume failed")}
	stateless := newBackend(sm, key)
	inc, err = stateless.Fork(context.Background(), api.SnapshotRef{Local: "parent"}, api.ForkOpts{ChildSessionUID: "child"})
	childRef := stateless.CleanupResources(api.CleanupFork, "child")[0].Reference
	if got := inc.Cleanup.Resources(); err == nil || len(got) != 1 || got[0].Reference != childRef || got[0].UID == "" {
		t.Fatalf("stateless fork: err=%v claim=%+v, want the child's actor claim", err, got)
	}
	if !reflect.DeepEqual(sm.calls[len(sm.calls)-2:], []string{"suspend:child", "delete:child"}) {
		t.Fatalf("calls=%v, want main's teardown of the child", sm.calls)
	}
}
