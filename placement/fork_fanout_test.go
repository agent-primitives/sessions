package placement_test

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/harness/echoagent"
	"github.com/aramase/agentsessions/placement"
	"github.com/aramase/agentsessions/sqlitelog"
)

// forkChildren forks the session at its head into n children named <uid>-child-1 … <uid>-child-n.
func forkChildren(t *testing.T, p *placement.Placer, log *sqlitelog.Log, uid string, n int) error {
	t.Helper()
	head, err := log.Head()
	if err != nil {
		return err
	}
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	children := make([]placement.ForkChild, 0, n)
	for i := 1; i <= n; i++ {
		child := fmt.Sprintf("%s-child-%d", uid, i)
		children = append(children, placement.ForkChild{UID: child, Log: store.Session(child)})
	}
	return p.Fork(context.Background(), log, uid, children, head)
}

// execAtHead runs one echo turn on the session at its current head.
func execAtHead(p *placement.Placer, log *sqlitelog.Log, uid string) error {
	head, err := log.Head()
	if err != nil {
		return err
	}
	_, err = p.Exec(context.Background(), log, uid, []api.Message{*api.TextMessage("user", "hi")}, head)
	return err
}

// A stateful fork's children are cloned from the snapshot the parent holds. A turn that wakes the
// parent before the last child is cloned replaces that snapshot, so the parent stays refused, on
// every Placer of the Registry (static or added with Registry.Add), until the whole fan-out is done.
func TestForkRefusesAParentTurnUntilTheLastChildIsCloned(t *testing.T) {
	for _, tc := range []struct {
		name string
		pair placerPair
	}{
		{"same placer", samePlacer},
		{"registry", registryPair},
		{"added placer", addedPair},
		{"through an added placer", addedPairReversed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hs := startHarnessServer(t, echoagent.Harness{})
			store, err := sqlitelog.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			log := store.Session("s1")

			backend := &routedBackend{address: hs.addr, desc: memDescriptor}
			p, turn := tc.pair(t, backend, echoagent.Model)
			var execErr error
			backend.onFork = func(child string) {
				if child == "s1-child-2" {
					execErr = execAtHead(turn, log, "s1")
				}
			}
			if err := forkChildren(t, p, log, "s1", 2); err != nil {
				t.Fatalf("fork: %v", err)
			}
			if !errors.Is(execErr, placement.ErrCheckpointing) {
				t.Fatalf("turn on the parent while the last child was cloned returned %v, want ErrCheckpointing", execErr)
			}
			if n := len(hs.started); n != 0 {
				t.Fatalf("%d harness stream(s) opened on the parent during the fan-out", n)
			}
			// Once the fan-out is done, the parent takes turns again.
			if err := execAtHead(turn, log, "s1"); err != nil {
				t.Fatalf("turn on the parent after the fork: %v", err)
			}
		})
	}
}

// A fan-out that fails partway tears its children down, one Stop at a time with a budget each.
// Nothing in the teardown reads the parent, so the parent is released first rather than refused for
// the length of the teardown.
func TestForkReleasesTheParentBeforeStoppingTheChildren(t *testing.T) {
	hs := startHarnessServer(t, echoagent.Harness{})
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	log := store.Session("s1")

	cloneErr := errors.New("clone failed")
	backend := &routedBackend{address: hs.addr, desc: memDescriptor, forkErr: map[string]error{"s1-child-2": cloneErr}}
	p := placement.New(backend, echoagent.Model)
	stops := 0
	var execErr error
	backend.onStop = func() {
		stops++
		if stops == 1 {
			execErr = execAtHead(p, log, "s1")
		}
	}
	if err := forkChildren(t, p, log, "s1", 2); !errors.Is(err, cloneErr) {
		t.Fatalf("fork returned %v, want the clone error", err)
	}
	if stops != 1 {
		t.Fatalf("teardown stopped %d children, want the 1 that was cloned", stops)
	}
	if execErr != nil {
		t.Fatalf("turn on the parent during the failed fork's teardown returned %v, want it to run", execErr)
	}
}

// Checkpoints of one session do not nest. A second stateful fork that arrives while the first one
// checkpoints the parent, or clones its children, is refused before it mints a fence or snapshots:
// its fence would fail the first fork's SUSPEND record after the parent is already cold, and its
// snapshot would replace the one the first fork's remaining children clone from.
func TestForkRefusesAForkDuringAnotherForksCheckpoint(t *testing.T) {
	for _, tc := range []struct {
		name  string
		phase func(b *routedBackend, nested func())
	}{
		{"checkpoint", func(b *routedBackend, nested func()) { b.onSnapshot = nested }},
		{"fan-out", func(b *routedBackend, nested func()) {
			b.onFork = func(child string) {
				if child == "s1-child-1" {
					nested()
				}
			}
		}},
	} {
		for _, pc := range []struct {
			name string
			pair placerPair
		}{
			{"same placer", samePlacer},
			{"registry", registryPair},
			{"added placer", addedPair},
			{"through an added placer", addedPairReversed},
		} {
			t.Run(tc.name+"/"+pc.name, func(t *testing.T) {
				store, err := sqlitelog.Open(":memory:")
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				log := store.Session("s1")

				backend := &routedBackend{desc: memDescriptor}
				p, other := pc.pair(t, backend, echoagent.Model)
				var nestedErr error
				ran := false
				tc.phase(backend, func() {
					if ran {
						return // a nested fork that is not refused re-enters this hook
					}
					ran = true
					nestedErr = forkChildren(t, other, log, "s1", 1)
				})
				if err := forkChildren(t, p, log, "s1", 2); err != nil {
					t.Fatalf("fork: %v", err)
				}
				if !errors.Is(nestedErr, placement.ErrCheckpointing) {
					t.Fatalf("fork during another fork's %s returned %v, want ErrCheckpointing", tc.name, nestedErr)
				}
				if backend.snapshots != 1 {
					t.Fatalf("the parent was checkpointed %d times, want once", backend.snapshots)
				}
			})
		}
	}
}
