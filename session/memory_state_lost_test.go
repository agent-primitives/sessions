package session_test

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
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

// A session whose actor disappeared after a turn (deleted out of band, or a host restarted
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
		for _, want := range []string{sess, "1 started turn(s) on harness \"mem\"", "cannot be reconstructed"} {
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

// failingHarness is the echo harness with a turn that fails after it has started, the way a harness
// fails part way through, having possibly changed its in-memory state.
type failingHarness struct{ echoagent.Harness }

var errTurnFailed = errors.New("turn failed part way")

func (failingHarness) Run(context.Context, *api.Start, api.EventSink) error { return errTurnFailed }

// newMemoryHost serves one REQUIRES_MEMORY_SNAPSHOT substrate harness, "mem", on ctl and store, as one
// agentsessionsd host would; the harness each turn reaches is har.
func newMemoryHost(t *testing.T, ctl *actorControl, store *sqlitelog.Store, har api.Harness) v1.SessionsClient {
	t.Helper()
	desc, err := echoagent.Harness{}.Describe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	desc.ID = "mem"
	desc.Capabilities.Resumability = api.ResumabilityRequiresMemorySnapshot
	backend := substrate.New(ctl, "space", substrate.ObjectRef{Atespace: "space", Name: "mem"}, desc)
	dial := placement.WithDialer(func(api.Incarnation) (api.Harness, func() error, error) {
		return har, func() error { return nil }, nil
	})
	r, err := placement.NewRegistry("mem", map[string]*placement.Placer{"mem": placement.New(backend, echoagent.Model, dial)})
	if err != nil {
		t.Fatal(err)
	}
	return serveRegistry(t, store, r)
}

// A turn that started and then failed may have changed the harness's RAM before it failed, so it
// counts as much as a completed one: when the only turn of a memory-tier session ended in ERROR and
// its actor is lost, the next Exec is refused with FAILED_PRECONDITION, creates no actor and journals
// nothing, rather than continue the journal (which holds that turn's input) on a fresh actor.
func TestExecAfterAFailedTurnAndALostActorIsRefused(t *testing.T) {
	ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c := newMemoryHost(t, ctl, store, failingHarness{})
	sess := mustCreate(t, c)
	if err := execHi(c, sess); err == nil {
		t.Fatal("the first turn succeeded; want it to fail")
	}
	if err := ctl.DeleteActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: sess}); err != nil {
		t.Fatal(err)
	}
	head, err := store.Session(sess).Head()
	if err != nil {
		t.Fatal(err)
	}

	err = execHi(c, sess)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("turn after a failed turn and a lost actor: want FailedPrecondition, got %v", err)
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "1 started turn(s)") {
		t.Errorf("error %q does not report the failed turn", msg)
	}
	ctl.mu.Lock()
	_, recreated := ctl.status[sess]
	ctl.mu.Unlock()
	if recreated {
		t.Error("the refused turn created an actor for the session")
	}
	if got, err := store.Session(sess).Head(); err != nil || got != head {
		t.Errorf("journal head after the refused turn = %d (err %v), want %d: nothing may be journaled", got, err, head)
	}
}

// Two hosts share the journal and the atespace. Host A starts the session's first turn and, before
// its Create looks for the actor, host B runs the first turn on a new actor, which is then deleted.
// A finds no actor. Had A counted the session's turns before looking (0), it would create a fresh
// actor, and once its stale append failed the next turn would attach to that actor and continue the
// journal without B's turn's state. The count is taken after GetActor reports the actor missing, so
// A sees B's turn and refuses with FAILED_PRECONDITION, creating nothing.
func TestTurnRecordedByAnotherHostBeforeTheActorIsLostIsCounted(t *testing.T) {
	ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hostA := newMemoryHost(t, ctl, store, echoagent.Harness{})
	hostB := newMemoryHost(t, ctl, store, echoagent.Harness{})
	sess := mustCreate(t, hostA)

	var fired atomic.Bool
	var hostBErr error
	ctl.onGet = func(actor string) {
		// Only host A's first look runs this; host B's own GetActor calls pass straight through.
		if actor != sess || !fired.CompareAndSwap(false, true) {
			return
		}
		// Host A is inside Create, about to look for the actor. Host B runs the first turn, then
		// the actor is lost.
		hostBErr = errors.Join(execHi(hostB, sess),
			ctl.DeleteActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: sess}))
	}
	err = execHi(hostA, sess)
	ctl.onGet = nil
	if hostBErr != nil {
		t.Fatalf("host B's turn: %v", hostBErr)
	}
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("host A's turn after host B's turn and a lost actor: want FailedPrecondition, got %v", err)
	}
	ctl.mu.Lock()
	_, recreated := ctl.status[sess]
	ctl.mu.Unlock()
	if recreated {
		t.Error("host A created an actor over the state host B's turn built")
	}
}

// completedTurns counts the END events in a session's journal.
func completedTurns(t *testing.T, store *sqlitelog.Store, sess string) int {
	t.Helper()
	recs, err := store.Session(sess).Read(1)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, r := range recs {
		if r.Event.Kind == api.EventEnd {
			n++
		}
	}
	return n
}

// The interleaving the started-turn count cannot see. Host A's Create finds no actor and counts no
// started turn. Before A's CreateActor, host B creates the actor, journals and runs the session's
// first turn, and that actor is deleted out of band. A's CreateActor then succeeds, so an actor holding
// none of B's state now sits under the session's name. Nothing deletes it, and every later Create
// attaches to it without counting. B's turn recorded its actor's UID in EXECUTION_START, so A's turn and
// every later one, on either host, is refused with FAILED_PRECONDITION and journals nothing.
func TestActorCreatedAfterTheCountOverALostActorIsRefused(t *testing.T) {
	ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	hostA := newMemoryHost(t, ctl, store, echoagent.Harness{})
	hostB := newMemoryHost(t, ctl, store, echoagent.Harness{})
	sess := mustCreate(t, hostA)

	var fired atomic.Bool
	var hostBErr error
	ctl.onCreate = func(actor string) {
		// Only host A's CreateActor runs this: A has looked for the actor and counted no started turn.
		if actor != sess || !fired.CompareAndSwap(false, true) {
			return
		}
		hostBErr = errors.Join(execHi(hostB, sess),
			ctl.DeleteActor(context.Background(), substrate.ActorRef{Atespace: "space", Name: sess}))
	}
	errA := execHi(hostA, sess)
	ctl.onCreate = nil
	if hostBErr != nil {
		t.Fatalf("host B's first turn: %v", hostBErr)
	}
	if !fired.Load() {
		t.Fatal("host A never reached CreateActor")
	}
	ctl.mu.Lock()
	_, replaced := ctl.status[sess]
	ctl.mu.Unlock()
	if !replaced {
		t.Fatal("host A did not create the replacement actor this test is about")
	}
	if status.Code(errA) != codes.FailedPrecondition {
		t.Fatalf("host A's turn on the replacement actor: want FailedPrecondition, got %v", errA)
	}
	head, err := store.Session(sess).Head()
	if err != nil {
		t.Fatal(err)
	}
	for name, host := range map[string]v1.SessionsClient{"host A": hostA, "host B": hostB} {
		err := execHi(host, sess)
		if status.Code(err) != codes.FailedPrecondition {
			t.Fatalf("%s's next turn on the replacement actor: want FailedPrecondition, got %v", name, err)
		}
		if msg := status.Convert(err).Message(); !strings.Contains(msg, sess) || !strings.Contains(msg, "replaced") {
			t.Errorf("%s: error %q does not name the session and the replaced instance", name, msg)
		}
	}
	if got, err := store.Session(sess).Head(); err != nil || got != head {
		t.Errorf("journal head after the refused turns = %d (err %v), want %d: nothing may be journaled", got, err, head)
	}
	if n := completedTurns(t, store, sess); n != 1 {
		t.Errorf("completed turns = %d, want only host B's", n)
	}
}

// Two hosts racing a session's first turn on a healthy actor is ordinary, and the loser must be able
// to retry: it gets OK or a retryable ABORTED, never FAILED_PRECONDITION, and afterwards both hosts
// keep running turns on the one actor.
func TestTwoHostsRacingTheFirstTurnKeepTheSessionHealthy(t *testing.T) {
	for _, tc := range []struct {
		name string
		// arm installs the hook through which host B's first turn runs inside host A's placement.
		arm func(ctl *actorControl, hook func(actor string))
	}{
		{
			// B creates the actor and finishes its turn after A found no actor and before A's
			// CreateActor, which then finds the name taken and attaches to B's actor.
			name: "B creates the actor before A does",
			arm:  func(ctl *actorControl, hook func(string)) { ctl.onCreate = hook },
		},
		{
			// A creates the actor; B finds it, resumes it and finishes its turn before A's own
			// resume returns and before A's turn is journaled.
			name: "B runs on the actor A created",
			arm:  func(ctl *actorControl, hook func(string)) { ctl.onResume = hook },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
			store, err := sqlitelog.Open(":memory:")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			hostA := newMemoryHost(t, ctl, store, echoagent.Harness{})
			hostB := newMemoryHost(t, ctl, store, echoagent.Harness{})
			sess := mustCreate(t, hostA)

			var fired atomic.Bool
			var hostBErr error
			tc.arm(ctl, func(actor string) {
				if actor != sess || !fired.CompareAndSwap(false, true) {
					return
				}
				hostBErr = execHi(hostB, sess)
			})
			errA := execHi(hostA, sess)
			ctl.onCreate, ctl.onResume = nil, nil
			if !fired.Load() {
				t.Fatal("host B's turn never ran inside host A's placement")
			}
			if hostBErr != nil {
				t.Fatalf("host B's first turn: %v", hostBErr)
			}
			if code := status.Code(errA); code != codes.OK && code != codes.Aborted {
				t.Fatalf("host A's racing first turn: want OK or Aborted, got %v", errA)
			}
			if status.Code(errA) == codes.Aborted {
				if err := execHi(hostA, sess); err != nil {
					t.Fatalf("host A's retry: %v", err)
				}
			}
			if err := execHi(hostB, sess); err != nil {
				t.Fatalf("host B's next turn: %v", err)
			}
			if n := completedTurns(t, store, sess); n != 3 {
				t.Errorf("completed turns = %d, want 3 (B's, A's and B's next)", n)
			}
			ctl.mu.Lock()
			created := ctl.created
			ctl.mu.Unlock()
			if created != 1 {
				t.Errorf("actors created = %d, want 1: both hosts must share the session's actor", created)
			}
		})
	}
}

// Resume of a suspended memory-tier session must not hand the session to an actor created under its
// name after its own actor was lost: the snapshot it would restore went with the lost actor, and the
// replacement holds none of the state. The recorded UID differs, so Resume is refused with
// FAILED_PRECONDITION and journals nothing.
func TestResumeOntoAReplacementActorIsRefused(t *testing.T) {
	ctl := &actorControl{status: map[string]substrate.ActorStatus{}, snapshots: map[string]string{}}
	store, err := sqlitelog.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	c := newMemoryHost(t, ctl, store, echoagent.Harness{})
	ctx := context.Background()
	sess := mustCreate(t, c)
	if err := execHi(c, sess); err != nil {
		t.Fatalf("first turn: %v", err)
	}
	if _, err := c.Suspend(ctx, &v1.SuspendRequest{Session: sess}); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	ref := substrate.ActorRef{Atespace: "space", Name: sess}
	if err := ctl.DeleteActor(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if err := ctl.CreateActor(ctx, ref, substrate.ObjectRef{Atespace: "space", Name: "mem"}); err != nil {
		t.Fatal(err)
	}
	head, err := store.Session(sess).Head()
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Resume(ctx, &v1.ResumeRequest{Session: sess})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("resume onto a replacement actor: want FailedPrecondition, got %v", err)
	}
	if got, err := store.Session(sess).Head(); err != nil || got != head {
		t.Errorf("journal head after the refused resume = %d (err %v), want %d", got, err, head)
	}
}
