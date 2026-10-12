package placement_test

import (
	"context"
	"testing"

	"github.com/aramase/agentsessions/harness/echoagent"
	"github.com/aramase/agentsessions/placement"
	"github.com/aramase/agentsessions/sqlitelog"
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
