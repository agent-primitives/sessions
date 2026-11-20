package substrate_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/runtime/substrate"
)

func TestCleanupResourcesUseOpaqueActorAndForkTagGraph(t *testing.T) {
	stateless := newBackend(&mockControl{}, substrate.WithCleanupKey("route-key"))
	actor := stateless.CleanupResources(api.CleanupCreate, "sess")
	if len(actor) != 1 || actor[0].Reference == "" || actor[0].UID != "" || actor[0].Borrower != "" {
		t.Fatalf("actor plan=%+v", actor)
	}
	memory := newMemoryBackend(&mockControl{}, substrate.WithCleanupKey("route-key"))
	fork := memory.CleanupResources(api.CleanupFork, "child")
	if err := api.ValidateCleanupPlan(fork); err != nil {
		t.Fatalf("fork plan: %v", err)
	}
	if len(fork) != 2 || fork[0].Borrower != fork[1].Reference {
		t.Fatalf("fork plan=%+v", fork)
	}
}

func TestCleanupKeyedCreateReturnsOwnedActorEvidence(t *testing.T) {
	m := &mockControl{info: running("worker-a")}
	b := newBackend(m, substrate.WithCleanupKey("route-key"))
	inc, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	if err != nil {
		t.Fatal(err)
	}
	resources := inc.Cleanup.Resources()
	plan := b.CleanupResources(api.CleanupCreate, "sess-x")
	if len(resources) != 1 || resources[0].Reference != plan[0].Reference || resources[0].UID == "" {
		t.Fatalf("cleanup=%+v plan=%+v", resources, plan)
	}
	if inc.ComputeInstance != "" {
		t.Fatalf("stateless compute instance=%q", inc.ComputeInstance)
	}
}

func TestCleanupKeylessStopByIDResolvesExactUID(t *testing.T) {
	m := &mockControl{}
	m.exists("sess-x", substrate.StatusRunning)
	b := newBackend(m)
	if err := b.Stop(context.Background(), api.Incarnation{ID: "sess-x"}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"get:sess-x", "delete:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want=%v", m.calls, want)
	}
}

func TestCleanupCreateReturnsEvidenceWithResumeError(t *testing.T) {
	cause := errors.New("resume failed")
	m := &mockControl{info: running("worker-a"), resumeErr: cause}
	b := newBackend(m, substrate.WithCleanupKey("route-key"))
	inc, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	if !errors.Is(err, cause) {
		t.Fatalf("create=%v want %v", err, cause)
	}
	if len(inc.Cleanup.Resources()) != 1 {
		t.Fatalf("partial cleanup=%+v", inc.Cleanup.Resources())
	}
}

func TestCleanupRestoreRequiresRecordedUIDMatch(t *testing.T) {
	m := &mockControl{info: running("worker-a")}
	m.exists("sess-x", substrate.StatusRunning)
	b := newMemoryBackend(m, substrate.WithCleanupKey("route-key"))
	ref := api.SnapshotRef{Local: "sess-x", ExternalURI: "snapshot", Memory: true}
	actorUID := m.uids["sess-x"]
	m.holds("sess-x", "snapshot")
	inc, err := b.RestoreInstance(context.Background(), ref, func() (string, error) { return actorUID, nil })
	if err != nil {
		t.Fatal(err)
	}
	if len(inc.Cleanup.Resources()) != 1 || inc.Cleanup.Resources()[0].UID != actorUID {
		t.Fatalf("restore cleanup=%+v", inc.Cleanup.Resources())
	}
	if _, err := b.RestoreInstance(context.Background(), ref, func() (string, error) { return "replacement", nil }); err == nil {
		t.Fatal("Restore accepted a mismatched recorded UID")
	}
}

func TestCleanupMissingKeyKeepsLegacyStopWithoutDurablePlan(t *testing.T) {
	m := &mockControl{info: running("worker-a")}
	b := newBackend(m)
	if plan := b.CleanupResources(api.CleanupCreate, "sess-x"); len(plan) != 0 {
		t.Fatalf("keyless plan=%+v", plan)
	}
	inc, err := b.Create(context.Background(), &api.SessionSpec{SessionUID: "sess-x"})
	if err != nil {
		t.Fatal(err)
	}
	if got := inc.Cleanup.Resources(); len(got) != 1 || got[0].UID == "" {
		t.Fatalf("keyless incarnation lacks exact ephemeral identity: %+v", got)
	}
	if err := b.Stop(context.Background(), inc); err != nil {
		t.Fatalf("keyless Stop: %v", err)
	}
	if want := []string{"get:sess-x", "create:sess-x", "resume:sess-x", "delete:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want=%v", m.calls, want)
	}
}

func TestCleanupStopDeletesRunningActorByUIDWithoutSuspend(t *testing.T) {
	m := &mockControl{}
	b := newBackend(m, substrate.WithCleanupKey("route-key"))
	plan := b.CleanupResources(api.CleanupCreate, "sess-x")
	owned := plan[0]
	owned.UID = "actor-uid"
	evidence, err := api.NewCleanupEvidence([]api.CleanupResource{owned})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(context.Background(), api.Incarnation{ID: "sess-x", Cleanup: evidence}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"delete:sess-x"}; !reflect.DeepEqual(m.calls, want) {
		t.Fatalf("calls=%v want=%v", m.calls, want)
	}
}

func TestCleanupStopLeavesReplacementAndTagUntouched(t *testing.T) {
	m := &mockControl{}
	m.exists("sess-x", substrate.StatusRunning)
	m.uids["sess-x"] = "replacement"
	b := newBackend(m, substrate.WithCleanupKey("route-key"))
	resource := b.CleanupResources(api.CleanupCreate, "sess-x")[0]
	resource.UID = "old-uid"
	evidence, err := api.NewCleanupEvidence([]api.CleanupResource{resource})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(context.Background(), api.Incarnation{ID: "sess-x", Cleanup: evidence}); err == nil {
		t.Fatal("UID-mismatched Stop succeeded")
	}
	if _, exists := m.actors["sess-x"]; !exists {
		t.Fatal("Stop deleted the replacement")
	}
}

func TestCleanupForkLostChildResponseKeepsExactTag(t *testing.T) {
	cause := errors.New("clone response lost")
	m := &mockCloner{mockControl: &mockControl{info: running("worker")}, cloneErr: cause}
	b := newMemoryBackend(m, substrate.WithCleanupKey("route-key"))
	ref := api.SnapshotRef{Local: "parent", ExternalURI: "snapshot", Memory: true}
	m.exists("parent", substrate.StatusSuspended)
	m.holds("parent", "snapshot")
	inc, err := b.Fork(context.Background(), ref, api.ForkOpts{ChildSessionUID: "child"})
	if !errors.Is(err, cause) {
		t.Fatalf("fork=%v want %v", err, cause)
	}
	resources := inc.Cleanup.Resources()
	if len(resources) != 1 || resources[0].UID == "" || resources[0].Borrower == "" {
		t.Fatalf("partial cleanup=%+v", resources)
	}
	for _, call := range m.calls {
		if call == "untag:fork-child" {
			t.Fatalf("ambiguous clone released tag: %v", m.calls)
		}
	}
}
