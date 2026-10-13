package session_test

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/aramase/agentsessions/api"
	v1 "github.com/aramase/agentsessions/api/genpb"
	"github.com/aramase/agentsessions/harness/echoagent"
	"github.com/aramase/agentsessions/placement"
	"github.com/aramase/agentsessions/runtime/local"
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

// The memory-tier guard counts only the turns its own harness ran. A turn ExecRequest.harness sent to
// another harness left nothing in the memory harness's actor, so a default turn after it creates the
// actor as on a new session. Once the memory harness has run a turn, losing its actor is refused
// however many override turns ran in between.
func TestMemoryStateLostCountsOnlyTheMemoryHarnessTurns(t *testing.T) {
	ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
	desc, err := echoagent.Harness{}.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc.ID = "mem"
	desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
	backend := substrate.New(ctl, "space", substrate.ObjectRef{Atespace: "space", Name: "mem"}, desc)
	dial := placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return echoagent.Harness{}, func() error { return nil }, nil
	})
	echo := local.New(echoagent.Harness{})
	t.Cleanup(func() { _ = echo.Close() })
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	r, err := placement.NewRegistry("mem", map[string]*placement.Placer{
		"mem":  placement.New(backend, echoagent.Model, dial),
		"echo": placement.New(echo, echoagent.Model),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := serveRegistry(t, store, r)
	ctx := context.Background()
	hasActor := func(sess string) bool {
		ctl.mu.Lock()
		defer ctl.mu.Unlock()
		_, ok := ctl.status[sess]
		return ok
	}
	loseActor := func(sess string) {
		t.Helper()
		if err := ctl.DeleteActor(ctx, substrate.ActorRef{Atespace: "space", Name: sess}); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("override turn then default turn creates the actor", func(t *testing.T) {
		sess := mustCreate(t, c)
		if err := execOn(ctx, c, sess, "echo"); err != nil {
			t.Fatalf("override turn: %v", err)
		}
		if hasActor(sess) {
			t.Fatal("the override turn created a substrate actor")
		}
		if err := execHi(c, sess); err != nil {
			t.Fatalf("default turn after an override turn: %v", err)
		}
		if !hasActor(sess) {
			t.Fatal("the default turn did not create the memory harness's actor")
		}
	})

	t.Run("default, override, lost actor, default is refused", func(t *testing.T) {
		sess := mustCreate(t, c)
		if err := execHi(c, sess); err != nil {
			t.Fatalf("first default turn: %v", err)
		}
		if err := execOn(ctx, c, sess, "echo"); err != nil {
			t.Fatalf("override turn: %v", err)
		}
		loseActor(sess)
		err := execHi(c, sess)
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("default turn after the actor was lost: want FailedPrecondition, got %v", err)
		}
		msg := status.Convert(err).Message()
		for _, want := range []string{sess, "1 committed turn(s) on harness \"mem\"", "cannot be reconstructed"} {
			if !strings.Contains(msg, want) {
				t.Errorf("error %q does not contain %q", msg, want)
			}
		}
		if hasActor(sess) {
			t.Error("the refused turn created an actor for the session")
		}
	})
}

// Resume of a suspended session restores its actor's own snapshot. When the actor is gone, a
// REQUIRES_MEMORY_SNAPSHOT session's state went with it: Resume is refused with FAILED_PRECONDITION,
// naming the session, creates no actor and journals nothing. A STATELESS_REPLAY session keeps the
// INTERNAL resume error it had before; this rule is only about RAM state.
func TestResumeAfterTheActorIsLost(t *testing.T) {
	for _, tc := range []struct {
		name string
		tier api.Resumability
		want codes.Code
	}{
		{name: "memory tier is refused", tier: api.ResumabilityRequiresMemorySnapshot, want: codes.FailedPrecondition},
		{name: "stateless tier is unchanged", tier: api.ResumabilityStatelessReplay, want: codes.Internal},
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
			ctx := context.Background()
			sess := mustCreate(t, c)
			if err := execHi(c, sess); err != nil {
				t.Fatalf("first turn: %v", err)
			}
			if _, err := c.Suspend(ctx, &v1.SuspendRequest{Session: sess}); err != nil {
				t.Fatalf("suspend: %v", err)
			}
			if err := ctl.DeleteActor(ctx, substrate.ActorRef{Atespace: "space", Name: sess}); err != nil {
				t.Fatal(err)
			}
			head, err := store.Session(sess).Head()
			if err != nil {
				t.Fatal(err)
			}

			_, err = c.Resume(ctx, &v1.ResumeRequest{Session: sess})
			if status.Code(err) != tc.want {
				t.Fatalf("resume after the actor was lost: want %v, got %v", tc.want, err)
			}
			ctl.mu.Lock()
			_, recreated := ctl.status[sess]
			ctl.mu.Unlock()
			if recreated {
				t.Error("the failed resume created an actor for the session")
			}
			if got, err := store.Session(sess).Head(); err != nil || got != head {
				t.Errorf("journal head after the failed resume = %d (err %v), want %d: nothing may be journaled", got, err, head)
			}
			if tc.want != codes.FailedPrecondition {
				return
			}
			msg := status.Convert(err).Message()
			for _, want := range []string{sess, "cannot be restored"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not contain %q", msg, want)
				}
			}
		})
	}
}
