package placement_test

import (
	"context"
	"testing"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/placement"
)

// addedPair runs the checkpoint on a static Placer and the turn on a Placer added later with
// Registry.Add, the way session.HarnessRegistry adds a runtime-registered harness.
func addedPair(t *testing.T, b placement.Backend, model func(context.Context, api.ModelRequest) (api.ModelResponse, error)) (*placement.Placer, *placement.Placer) {
	t.Helper()
	static, added := placement.New(b, model), placement.New(b, model)
	r, err := placement.NewRegistry("default", map[string]*placement.Placer{"default": static})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add("registered", added); err != nil {
		t.Fatal(err)
	}
	return static, added
}

// addedPairReversed runs the checkpoint on the added Placer and the turn on the static one.
func addedPairReversed(t *testing.T, b placement.Backend, model func(context.Context, api.ModelRequest) (api.ModelResponse, error)) (*placement.Placer, *placement.Placer) {
	static, added := addedPair(t, b, model)
	return added, static
}

// A Placer added to a running host shares the session guard: Suspend through the static harness
// is refused while a turn runs through the added one.
func TestSuspendRefusesAStreamOpenedByAnAddedPlacer(t *testing.T) {
	testSuspendRefusesOpenStream(t, addedPair)
}

// A Placer added to a running host shares the checkpoint state: a stateful fork through the
// static harness ends a parent turn running through the added one, and the reverse.
func TestForkEndsAParentStreamOpenedByAnAddedPlacer(t *testing.T) {
	testCheckpointEndsOpenStream(t, forkCheckpoint, addedPair)
}

func TestForkThroughAnAddedPlacerEndsAStaticPlacersStream(t *testing.T) {
	testCheckpointEndsOpenStream(t, forkCheckpoint, addedPairReversed)
}

func TestForkRefusesAParentTurnOnAnAddedPlacer(t *testing.T) {
	testCheckpointRefusesANewTurn(t, forkCheckpoint, placement.ErrCheckpointing, addedPair)
}

// Add refuses a Placer that already shares another registry's state, and leaves it there: moving
// it would split the guard and checkpoint state of sessions it runs.
func TestRegistryAddRejectsAPlacerInAnotherRegistry(t *testing.T) {
	p := newPlacer(t)
	if _, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p}); err != nil {
		t.Fatal(err)
	}
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": newPlacer(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add("moved", p); err == nil {
		t.Fatal("Add accepted a placer that belongs to another registry")
	}
	if r.Has("moved") {
		t.Fatal("a refused Add left the name resolvable")
	}
	// The refused placer still belongs to its own registry, so a third one refuses it too.
	if _, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p}); err == nil {
		t.Fatal("the refused placer was detached from its registry")
	}
}

// The same Placer under a second name is one Placer: Add accepts it and both names resolve to it.
func TestRegistryAddAcceptsAPlacerAlreadyInTheRegistry(t *testing.T) {
	p := newPlacer(t)
	r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": p})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Add("alias", p); err != nil {
		t.Fatal(err)
	}
	if got, err := r.For("alias"); err != nil || got != p {
		t.Fatalf("For(alias) = %p, %v; want the static placer", got, err)
	}
}
