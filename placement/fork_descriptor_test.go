package placement_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/sqlitelog"
)

// The descriptor id gate runs before a stateful fork's checkpoint. A fork through a Placer that
// expects another harness is refused with ErrDescriptorMismatch and leaves the parent alone: no
// snapshot, no fence, and a turn already running on the parent through another Placer of the
// Registry keeps its stream and finishes.
func TestStatefulForkRefusedForADescriptorMismatchLeavesTheParentTurnRunning(t *testing.T) {
	hs := startHarnessServer(t, actorHarness{actor: "space/s1"})
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	log := store.Session("s1")

	modelCalled := make(chan struct{})
	release := make(chan struct{})
	model := func(ctx context.Context, req api.ModelRequest) (api.ModelResponse, error) {
		close(modelCalled)
		select {
		case <-release:
		case <-ctx.Done():
			return api.ModelResponse{}, ctx.Err()
		}
		return api.ModelResponse{Message: *api.TextMessage("assistant", "done")}, nil
	}
	backend := &routedBackend{address: hs.addr, desc: memDescriptor}
	mismatched := placement.New(backend, model, placement.WithDescriptorID("not-"+memDescriptor.ID))
	turn := placement.New(backend, model)
	if _, err := placement.NewRegistry("mismatched", map[string]*placement.Placer{"mismatched": mismatched, "turn": turn}); err != nil {
		t.Fatal(err)
	}

	execErr := make(chan error, 1)
	go func() {
		_, err := turn.Exec(context.Background(), log, "s1", []api.Message{*api.TextMessage("user", "hi")}, 0)
		execErr <- err
	}()
	select {
	case <-modelCalled:
	case err := <-execErr:
		t.Fatalf("turn returned before parking on the model call: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("turn never reached the model call")
	}
	before := journalKinds(t, log)

	if err := forkCheckpoint(mismatched, log, "s1"); !errors.Is(err, placement.ErrDescriptorMismatch) {
		t.Fatalf("fork through a mismatched placer: want ErrDescriptorMismatch, got %v", err)
	}
	backend.mu.Lock()
	snapshots := backend.snapshots
	backend.mu.Unlock()
	if snapshots != 0 {
		t.Fatalf("a refused fork took %d snapshots of the parent", snapshots)
	}
	if after := journalKinds(t, log); !slices.Equal(after, before) {
		t.Fatalf("a refused fork changed the parent journal from %v to %v", before, after)
	}

	close(release)
	select {
	case err := <-execErr:
		if err != nil {
			t.Fatalf("the parent turn was disturbed by a refused fork: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the parent turn never finished")
	}
}
