package placement_test

import (
	"context"
	"errors"
	"testing"

	"github.com/agent-primitives/sessions/harness/echoagent"
	"github.com/agent-primitives/sessions/placement"
	"github.com/agent-primitives/sessions/sqlitelog"
)

var checkpointPairs = []struct {
	name string
	pair placerPair
}{
	{"same placer", samePlacer},
	{"registry", registryPair},
	{"added placer", addedPair},
	{"through an added placer", addedPairReversed},
}

func suspendParent(p *placement.Placer, log *sqlitelog.Log, uid string) error {
	_, err := p.Suspend(context.Background(), log, uid)
	return err
}

// panics reports whether f panicked, and recovers the panic as a caller embedding the Placer might.
func panics(f func()) (panicked bool) {
	defer func() {
		if recover() != nil {
			panicked = true
		}
	}()
	f()
	return false
}

// A runtime whose Snapshot panics must not leave the session's checkpoint mark behind. A caller
// that recovers the panic gets back a session that takes turns, forks and suspends again.
func TestACheckpointThatPanicsReleasesTheSession(t *testing.T) {
	for _, tc := range []struct {
		name       string
		checkpoint func(t *testing.T, p *placement.Placer, log *sqlitelog.Log) error
	}{
		{"fork", func(t *testing.T, p *placement.Placer, log *sqlitelog.Log) error {
			return forkChildren(t, p, log, "s1", 1)
		}},
		{"suspend", func(_ *testing.T, p *placement.Placer, log *sqlitelog.Log) error { return suspendParent(p, log, "s1") }},
	} {
		for _, pc := range checkpointPairs {
			t.Run(tc.name+"/"+pc.name, func(t *testing.T) {
				hs := startHarnessServer(t, echoagent.Harness{})
				store, err := sqlitelog.Open(":memory:")
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				log := store.Session("s1")

				backend := &routedBackend{address: hs.addr, desc: memDescriptor}
				p, turn := pc.pair(t, backend, echoagent.Model)
				backend.onSnapshot = func() { panic("runtime checkpoint panicked") }
				if !panics(func() { _ = tc.checkpoint(t, p, log) }) {
					t.Fatalf("%s did not panic", tc.name)
				}
				backend.onSnapshot = nil

				if err := execAtHead(turn, log, "s1"); err != nil {
					t.Fatalf("turn after the %s panicked: %v", tc.name, err)
				}
				if err := forkChildren(t, turn, log, "s1", 1); err != nil {
					t.Fatalf("fork after the %s panicked: %v", tc.name, err)
				}
				if err := suspendParent(turn, log, "s1"); err != nil {
					t.Fatalf("suspend after the %s panicked: %v", tc.name, err)
				}
			})
		}
	}
}

// A Suspend and a stateful fork of one session hold the same checkpoint mark, so neither runs while
// the other holds it. A Suspend beside a fork would otherwise mint a fence that fails the fork's
// SUSPEND record after the parent is already cold, or replace the snapshot the fork's remaining
// children clone from; a fork beside a Suspend would fail the Suspend's record the same way. The
// one that arrives second is refused before it fences or snapshots anything, the first completes,
// and a retry once it has returned succeeds.
func TestSuspendAndAStatefulForkDoNotOverlap(t *testing.T) {
	for _, tc := range []struct {
		name  string
		outer func(t *testing.T, p *placement.Placer, log *sqlitelog.Log) error
		inner func(t *testing.T, p *placement.Placer, log *sqlitelog.Log) error
		phase func(b *routedBackend, nested func())
	}{
		{
			name: "suspend during a fork's checkpoint",
			outer: func(t *testing.T, p *placement.Placer, log *sqlitelog.Log) error {
				return forkChildren(t, p, log, "s1", 2)
			},
			inner: func(_ *testing.T, p *placement.Placer, log *sqlitelog.Log) error { return suspendParent(p, log, "s1") },
			phase: func(b *routedBackend, nested func()) { b.onSnapshot = nested },
		},
		{
			name: "suspend during a fork's fan-out",
			outer: func(t *testing.T, p *placement.Placer, log *sqlitelog.Log) error {
				return forkChildren(t, p, log, "s1", 2)
			},
			inner: func(_ *testing.T, p *placement.Placer, log *sqlitelog.Log) error { return suspendParent(p, log, "s1") },
			phase: func(b *routedBackend, nested func()) {
				b.onFork = func(child string) {
					if child == "s1-child-1" {
						nested()
					}
				}
			},
		},
		{
			name:  "fork during a suspend",
			outer: func(_ *testing.T, p *placement.Placer, log *sqlitelog.Log) error { return suspendParent(p, log, "s1") },
			inner: func(t *testing.T, p *placement.Placer, log *sqlitelog.Log) error {
				return forkChildren(t, p, log, "s1", 1)
			},
			phase: func(b *routedBackend, nested func()) { b.onSnapshot = nested },
		},
	} {
		for _, pc := range checkpointPairs {
			t.Run(tc.name+"/"+pc.name, func(t *testing.T) {
				store, err := sqlitelog.Open(":memory:")
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				log := store.Session("s1")

				backend := &routedBackend{desc: memDescriptor}
				p, other := pc.pair(t, backend, echoagent.Model)
				var innerErr error
				ran := false
				tc.phase(backend, func() {
					if ran {
						return // an inner operation that is not refused re-enters this hook
					}
					ran = true
					innerErr = tc.inner(t, other, log)
				})
				if err := tc.outer(t, p, log); err != nil {
					t.Fatalf("the operation that held the checkpoint failed: %v", err)
				}
				if !ran {
					t.Fatal("the inner operation never ran")
				}
				if !errors.Is(innerErr, placement.ErrCheckpointing) {
					t.Fatalf("%s returned %v, want ErrCheckpointing", tc.name, innerErr)
				}
				if backend.snapshots != 1 {
					t.Fatalf("the session was checkpointed %d times, want once", backend.snapshots)
				}
				backend.onSnapshot, backend.onFork = nil, nil
				if err := tc.inner(t, other, log); err != nil {
					t.Fatalf("retry once the checkpoint returned: %v", err)
				}
			})
		}
	}
}
