package session_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/aramase/agentsessions/api"
	"github.com/aramase/agentsessions/harness/echoagent"
	"github.com/aramase/agentsessions/placement"
	"github.com/aramase/agentsessions/runtime/substrate"
	"github.com/aramase/agentsessions/sqlitelog"
)

// A session whose actor disappeared after a committed turn (deleted out of band, or a host restarted
// against another atespace) has no compute to attach to and nothing to restore. For a
// REQUIRES_MEMORY_SNAPSHOT harness the next Exec must be refused with FAILED_PRECONDITION, naming the
// session, and must not create an actor: a fresh one would continue the journal without the state the
// recorded turns built. A STATELESS_REPLAY harness rebuilds from the journal, so it keeps working.
func TestExecAfterTheActorIsLost(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tier       api.Resumability
		wantRefuse bool
	}{
		{name: "memory tier is refused", tier: api.ResumabilityRequiresMemorySnapshot, wantRefuse: true},
		{name: "stateless tier creates a new actor", tier: api.ResumabilityStatelessReplay},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
			desc, err := echoagent.Harness{}.Describe(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			desc.Capabilities.Resumability = tc.tier
			backend := substrate.New(ctl, "space", substrate.ObjectRef{Atespace: "space", Name: "echo"}, desc)
			dial := placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
				return echoagent.Harness{}, func() error { return nil }, nil
			})
			store, err := sqlitelog.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			r, err := placement.NewRegistry("echo", map[string]*placement.Placer{"echo": placement.New(backend, echoagent.Model, dial)})
			if err != nil {
				t.Fatal(err)
			}
			c := serveRegistry(t, store, r)
			sess := mustCreate(t, c)

			if err := execHi(c, sess); err != nil {
				t.Fatalf("first turn: %v", err)
			}
			if err := ctl.DeleteActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: sess}); err != nil {
				t.Fatal(err)
			}
			head, err := store.Session(sess).Head()
			if err != nil {
				t.Fatal(err)
			}

			err = execHi(c, sess)
			ctl.mu.Lock()
			_, recreated := ctl.status[sess]
			ctl.mu.Unlock()
			if !tc.wantRefuse {
				if err != nil || !recreated {
					t.Fatalf("second turn after the actor was lost: err=%v recreated=%v, want a new actor and success", err, recreated)
				}
				return
			}
			if status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("second turn after the actor was lost: want FailedPrecondition, got %v", err)
			}
			msg := status.Convert(err).Message()
			for _, want := range []string{sess, "cannot be reconstructed"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not contain %q", msg, want)
				}
			}
			if recreated {
				t.Error("the refused turn created an actor for the session")
			}
			if got, err := store.Session(sess).Head(); err != nil || got != head {
				t.Errorf("journal head after the refused turn = %d (err %v), want %d: nothing may be journaled", got, err, head)
			}
		})
	}
}
