package claudecode

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/agent-primitives/sessions/api"
)

// The tests here run the parent's and the child's turns on one harness: its memory then holds what
// a memory-tier fork's clone holds, the parent's entry keyed by the parent's UID.

// turnEvents is what the host journals for a completed turn: its start and input, then what the
// harness reported.
func turnEvents(exec string) []api.Event {
	return []api.Event{
		{ExecutionID: exec, Kind: api.EventExecutionStart, ExecutionStart: &api.ExecutionStart{}},
		{ExecutionID: exec, Kind: api.EventInput, Message: api.TextMessage("user", "go")},
		{ExecutionID: exec, Kind: api.EventOutput, Message: api.TextMessage("assistant", "Hello, world.")},
		{ExecutionID: exec, Kind: api.EventEnd},
	}
}

// forkMarker is the child's first record after the copied prefix (controller.Fork).
func forkMarker() []api.Event {
	return []api.Event{{Kind: api.EventLifecycle, Lifecycle: &api.Lifecycle{Kind: api.LifecycleFork, Detail: "parent@2"}}}
}

// lastSessionFlags returns the session flags of the fake's last start. With Config{} they end the
// argv, after the ten fixed arguments.
func lastSessionFlags(t *testing.T, h *Harness) []string {
	t.Helper()
	got := starts(t, h)
	if len(got) == 0 {
		t.Fatal("no process started")
	}
	return got[len(got)-1].Args[10:]
}

func wantFlags(t *testing.T, h *Harness, want ...string) {
	t.Helper()
	if got := lastSessionFlags(t, h); !slices.Equal(got, want) {
		t.Fatalf("session flags = %q, want %q", got, want)
	}
}

func sessionSnapshot(h *Harness, uid string) (sessionState, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	st, ok := h.sessions[uid]
	if !ok {
		return sessionState{}, false
	}
	return *st, true
}

// setRunning marks a session's turn in flight, or not, as begin and end do.
func setRunning(h *Harness, uid string, running bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[uid].running = running
}

// forkReaders returns how many fork-child attempts hold uid reserved.
func forkReaders(h *Harness, uid string) int {
	st, _ := sessionSnapshot(h, uid)
	return st.forkReaders
}

// pausedTurn starts a turn whose fake claude emits nothing and blocks, as a fork child's process
// does before Claude Code has read the transcript it forks, and waits until it blocks. It then sets
// the scenario back to a text turn for the turns that follow. stop cancels the turn and returns
// its error; cleanup stops it if the test did not.
func pausedTurn(t *testing.T, h *Harness, uid, exec string, history []api.Event) (stop func() error) {
	t.Helper()
	blocked := func() (n int) {
		for _, r := range records(t, h) {
			if r.Event == "blocking" {
				n++
			}
		}
		return n
	}
	before := blocked()
	setScenario(t, h, scenario{Block: true})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx, startFor(uid, exec, history, "go"), &recordingSink{t: t}) }()
	var stopped bool
	var err error
	stop = func() error {
		if !stopped {
			cancel()
			err, stopped = <-done, true
		}
		return err
	}
	t.Cleanup(func() { stop() })
	for deadline := time.Now().Add(10 * time.Second); blocked() == before; time.Sleep(10 * time.Millisecond) {
		select {
		case err := <-done:
			stopped = true
			t.Fatalf("%s turn %s ended before it blocked: %v", uid, exec, err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s turn %s never blocked", uid, exec)
		}
	}
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	return stop
}

// forkInFlightTurn runs a turn that must be refused because a fork of its session is in flight:
// nothing spawns and the session's state is unchanged.
func forkInFlightTurn(t *testing.T, h *Harness, uid, exec string, history ...[]api.Event) {
	t.Helper()
	before, _ := sessionSnapshot(h, uid)
	spawned := len(starts(t, h))
	if err := runTurn(t, h, uid, exec, history...); !errors.Is(err, errForkInFlight) {
		t.Fatalf("%s turn %s = %v, want %v", uid, exec, err, errForkInFlight)
	}
	if got := len(starts(t, h)); got != spawned {
		t.Fatalf("spawned %d processes, want %d", got, spawned)
	}
	if got, _ := sessionSnapshot(h, uid); got != before {
		t.Fatalf("%s state = %+v, want %+v unchanged", uid, got, before)
	}
}

func runTurn(t *testing.T, h *Harness, uid, exec string, history ...[]api.Event) error {
	t.Helper()
	return h.Run(context.Background(), startFor(uid, exec, slices.Concat(history...), "go"), &recordingSink{t: t})
}

// refusedTurn runs a turn whose ctx is already done: begin refuses it before recording the
// execution as started, and nothing runs.
func refusedTurn(t *testing.T, h *Harness, uid, exec string, history []api.Event) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.Run(ctx, startFor(uid, exec, history, "go"), &recordingSink{t: t}); !errors.Is(err, context.Canceled) {
		t.Fatalf("%s turn %s = %v, want context.Canceled", uid, exec, err)
	}
}

// TestForkChildAdoptsParentSession: a fork child with no state of its own forks the parent's
// Claude Code session into its own id, then resumes its own; the parent's state and session are
// untouched.
func TestForkChildAdoptsParentSession(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := runTurn(t, h, "parent", "e1"); err != nil {
		t.Fatal(err)
	}
	parent, _ := sessionSnapshot(h, "parent")

	sink := &recordingSink{t: t}
	if err := h.Run(context.Background(), startFor("child", "e2", slices.Concat(turnEvents("e1"), forkMarker()), "go"), sink); err != nil {
		t.Fatal(err)
	}
	wantFlags(t, h, "--resume", sessionID("parent", 0), "--fork-session", "--session-id", sessionID("child", 0))
	if !slices.Equal(sink.outputs, []string{"Hello, world."}) {
		t.Fatalf("child outputs = %q", sink.outputs)
	}
	if got, _ := sessionSnapshot(h, "parent"); got != parent {
		t.Fatalf("parent state = %+v, want %+v unchanged", got, parent)
	}

	if err := runTurn(t, h, "child", "e3", turnEvents("e1"), forkMarker(), turnEvents("e2")); err != nil {
		t.Fatal(err)
	}
	wantFlags(t, h, "--resume", sessionID("child", 0))
	if err := runTurn(t, h, "parent", "e4", turnEvents("e1")); err != nil {
		t.Fatal(err)
	}
	wantFlags(t, h, "--resume", sessionID("parent", 0))
}

// TestForkChildWithoutSingleParentFailsClosed: a session with no state whose History does not
// identify exactly one idle session as the one it forks from is refused as lost state, before
// anything runs, and no state is created for it.
func TestForkChildWithoutSingleParentFailsClosed(t *testing.T) {
	t.Parallel()
	forked := slices.Concat(turnEvents("e1"), forkMarker())
	for _, test := range []struct {
		name    string
		parent  bool // the parent ran e1 on the harness first
		setup   func(t *testing.T, h *Harness)
		history []api.Event
	}{
		{name: "no fork marker", parent: true, history: turnEvents("e1")},
		{name: "no session on the instance", history: forked},
		{name: "fork point not started here", parent: true, history: slices.Concat(turnEvents("e0"), forkMarker())},
		// The fork point is the last execution before the marker, never an older one this instance
		// started: e2 may have run elsewhere or on a process that died, so adopting e1's session
		// could continue an older conversation than the parent's last turn.
		{
			name:    "fork point ran elsewhere after a turn started here",
			parent:  true,
			history: slices.Concat(turnEvents("e1"), turnEvents("e2"), forkMarker()),
		},
		{
			name:    "fork point has no execution id",
			parent:  true,
			history: slices.Concat(turnEvents("e1"), turnEvents(""), forkMarker()),
		},
		// Owner-accepted consequence: begin refused e2 before recording it as started, and the
		// instance cannot tell that from an e2 that ran elsewhere.
		{
			name:    "fork point refused before it started",
			parent:  true,
			history: slices.Concat(turnEvents("e1"), turnEvents("e2"), forkMarker()),
			setup: func(t *testing.T, h *Harness) {
				refusedTurn(t, h, "parent", "e2", turnEvents("e1"))
			},
		},
		{
			name:    "fork point refused while another turn was running",
			parent:  true,
			history: slices.Concat(turnEvents("e1"), turnEvents("e2"), forkMarker()),
			setup: func(t *testing.T, h *Harness) {
				setRunning(h, "parent", true)
				if err := runTurn(t, h, "parent", "e2", turnEvents("e1")); !errors.Is(err, errBusy) {
					t.Fatalf("parent turn e2 = %v, want %v", err, errBusy)
				}
				setRunning(h, "parent", false)
			},
		},
		// An execution with no EXECUTION_START (EXE-31) is still an execution: its INPUT marks it,
		// as hasPriorTurn reads it, so it is the fork point rather than skipped for e1.
		{
			name:    "fork point has no execution start",
			parent:  true,
			history: slices.Concat(turnEvents("e1"), turnEvents("e2")[1:], forkMarker()),
		},
		{
			name:    "fork of a fork whose turn has no execution start",
			parent:  true,
			history: slices.Concat(forked, turnEvents("e2")[1:], forkMarker()),
		},
		{
			name:    "fork point is a legacy input with no execution id",
			parent:  true,
			history: slices.Concat(turnEvents("e1"), turnEvents("")[1:], forkMarker()),
		},
		// A fork of a fork follows the same rule as the first child: e2, the first child's turn, is
		// the fork point, and this instance holds no record of it having started.
		{
			name:    "fork of a fork whose turn never reached here",
			parent:  true,
			history: slices.Concat(forked, turnEvents("e2"), forkMarker()),
		},
		{
			name:    "fork of a fork whose turn was refused before it started",
			parent:  true,
			history: slices.Concat(forked, turnEvents("e2"), forkMarker()),
			setup: func(t *testing.T, h *Harness) {
				refusedTurn(t, h, "fork1", "e2", forked)
			},
		},
		{name: "parent ran after the fork point", parent: true, history: forked, setup: func(t *testing.T, h *Harness) {
			if err := runTurn(t, h, "parent", "e3", turnEvents("e1")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "child turn after the marker", parent: true, history: slices.Concat(forked, turnEvents("e2"))},
		{name: "legacy input after the marker", parent: true, history: slices.Concat(forked, turnEvents("e2")[1:])},
		{name: "two sessions match", parent: true, history: forked, setup: func(_ *testing.T, h *Harness) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.sessions["other"] = &sessionState{lastExec: "e1", established: true}
		}},
		{name: "parent turn in flight", parent: true, history: forked, setup: func(_ *testing.T, h *Harness) {
			setRunning(h, "parent", true)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
			if test.parent {
				if err := runTurn(t, h, "parent", "e1"); err != nil {
					t.Fatal(err)
				}
			}
			if test.setup != nil {
				test.setup(t, h)
			}
			parent, _ := sessionSnapshot(h, "parent")
			spawned := len(starts(t, h))

			if err := runTurn(t, h, "child", "e9", test.history); !errors.Is(err, errStateLost) {
				t.Fatalf("Run = %v, want %v", err, errStateLost)
			}
			if got := len(starts(t, h)); got != spawned {
				t.Fatalf("spawned %d processes, want %d", got, spawned)
			}
			if st, ok := sessionSnapshot(h, "child"); ok {
				t.Fatalf("child state created: %+v", st)
			}
			if got, _ := sessionSnapshot(h, "parent"); got != parent {
				t.Fatalf("parent state = %+v, want %+v unchanged", got, parent)
			}
		})
	}
}

// TestForkAfterParentRecovers: a fork whose fork point the instance refused is refused, but once
// the parent runs a later turn on the instance, a new fork has a known fork point and adopts.
func TestForkAfterParentRecovers(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := runTurn(t, h, "parent", "e1"); err != nil {
		t.Fatal(err)
	}
	refusedTurn(t, h, "parent", "e2", turnEvents("e1"))
	if err := runTurn(t, h, "child", "e3", turnEvents("e1"), turnEvents("e2"), forkMarker()); !errors.Is(err, errStateLost) {
		t.Fatalf("fork after refused turn = %v, want %v", err, errStateLost)
	}
	if err := runTurn(t, h, "parent", "e4", turnEvents("e1"), turnEvents("e2")); err != nil {
		t.Fatal(err)
	}
	history := slices.Concat(turnEvents("e1"), turnEvents("e2"), turnEvents("e4"), forkMarker())
	if err := runTurn(t, h, "child2", "e5", history); err != nil {
		t.Fatal(err)
	}
	wantFlags(t, h, "--resume", sessionID("parent", 0), "--fork-session", "--session-id", sessionID("child2", 0))
}

// TestForkChildFirstTurnFailsBeforeInit: a fork attempt that dies before init may have left a
// transcript under the child's id, so the next attempt forks into the child's next id.
func TestForkChildFirstTurnFailsBeforeInit(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := runTurn(t, h, "parent", "e1"); err != nil {
		t.Fatal(err)
	}
	setScenario(t, h, scenario{Exit: 1})
	if err := runTurn(t, h, "child", "e2", turnEvents("e1"), forkMarker()); err == nil {
		t.Fatal("failed turn returned nil")
	}
	wantFlags(t, h, "--resume", sessionID("parent", 0), "--fork-session", "--session-id", sessionID("child", 0))

	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := runTurn(t, h, "child", "e3", turnEvents("e1"), forkMarker(), turnEvents("e2")); err != nil {
		t.Fatal(err)
	}
	wantFlags(t, h, "--resume", sessionID("parent", 0), "--fork-session", "--session-id", sessionID("child", 1))
	if err := runTurn(t, h, "child", "e4", turnEvents("e1"), forkMarker(), turnEvents("e2"), turnEvents("e3")); err != nil {
		t.Fatal(err)
	}
	wantFlags(t, h, "--resume", sessionID("child", 1))
}

// TestForkChildCancelledBeforeStart: a child turn cancelled before it starts launches nothing but
// keeps the adopted state, so its retry still forks the parent's session.
func TestForkChildCancelledBeforeStart(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := runTurn(t, h, "parent", "e1"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	history := slices.Concat(turnEvents("e1"), forkMarker())
	if err := h.Run(ctx, startFor("child", "e2", history, "go"), &recordingSink{t: t}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if got := len(starts(t, h)); got != 1 {
		t.Fatalf("spawned %d processes, want 1", got)
	}
	if err := runTurn(t, h, "child", "e2", history); err != nil {
		t.Fatalf("retry = %v", err)
	}
	wantFlags(t, h, "--resume", sessionID("parent", 0), "--fork-session", "--session-id", sessionID("child", 0))
}

// TestForkOfUnconfirmedParent: a parent whose id Claude Code never confirmed has no conversation,
// so the child starts its own, as the parent's next turn would.
func TestForkOfUnconfirmedParent(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Exit: 1})
	if err := runTurn(t, h, "parent", "e1"); err == nil {
		t.Fatal("failed turn returned nil")
	}
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := runTurn(t, h, "child", "e2", turnEvents("e1"), forkMarker()); err != nil {
		t.Fatal(err)
	}
	wantFlags(t, h, "--session-id", sessionID("child", 0))
}

// TestForkOfFork: a grandchild forks from the session its parent, itself a fork child, runs under:
// the child's own once confirmed, else the one the child was forking from.
func TestForkOfFork(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		child    *scenario // the child's turn e2, if it ran
		history  []api.Event
		wantFrom string
	}{
		{
			name:     "child confirmed",
			child:    &scenario{Fixture: "text_turn.jsonl"},
			history:  slices.Concat(turnEvents("e1"), forkMarker(), turnEvents("e2"), forkMarker()),
			wantFrom: sessionID("child", 0),
		},
		{
			name:     "child failed before init",
			child:    &scenario{Exit: 1},
			history:  slices.Concat(turnEvents("e1"), forkMarker(), turnEvents("e2"), forkMarker()),
			wantFrom: sessionID("parent", 0),
		},
		{
			name:     "child never ran",
			history:  slices.Concat(turnEvents("e1"), forkMarker(), forkMarker()),
			wantFrom: sessionID("parent", 0),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
			if err := runTurn(t, h, "parent", "e1"); err != nil {
				t.Fatal(err)
			}
			if test.child != nil {
				setScenario(t, h, *test.child)
				if err := runTurn(t, h, "child", "e2", turnEvents("e1"), forkMarker()); (err == nil) != (test.child.Exit == 0) {
					t.Fatalf("child turn = %v, want failure %t", err, test.child.Exit != 0)
				}
				setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
			}
			if err := runTurn(t, h, "grandchild", "e3", test.history); err != nil {
				t.Fatal(err)
			}
			wantFlags(t, h, "--resume", test.wantFrom, "--fork-session", "--session-id", sessionID("grandchild", 0))
		})
	}
}

// TestForkPointWithoutExecutionStart: an execution with no EXECUTION_START (EXE-31) that this
// instance started is a fork point the child adopts, read from its INPUT.
func TestForkPointWithoutExecutionStart(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := runTurn(t, h, "parent", "e1"); err != nil {
		t.Fatal(err)
	}
	if err := runTurn(t, h, "child", "e2", turnEvents("e1")[1:], forkMarker()); err != nil {
		t.Fatal(err)
	}
	wantFlags(t, h, "--resume", sessionID("parent", 0), "--fork-session", "--session-id", sessionID("child", 0))
}

// TestForkChildRetryRevalidatesSource: until Claude Code confirms a fork child's own id, every
// attempt rechecks that the session it forks from is idle at the fork point. A source that ran on
// (its transcript now holds turns after the fork) or is running refuses the attempt as lost state,
// as a first attempt would be, before anything runs. Once the child's id is confirmed its turns
// resume its own transcript and the parent no longer matters.
func TestForkChildRetryRevalidatesSource(t *testing.T) {
	t.Parallel()
	forked := slices.Concat(turnEvents("e1"), forkMarker())
	cancelled := func(t *testing.T, h *Harness) { refusedTurn(t, h, "child", "e2", forked) }
	failedBeforeInit := func(t *testing.T, h *Harness) {
		setScenario(t, h, scenario{Exit: 1})
		if err := runTurn(t, h, "child", "e2", forked); err == nil {
			t.Fatal("failed child turn returned nil")
		}
		setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	}
	parentRanOn := func(t *testing.T, h *Harness) {
		if err := runTurn(t, h, "parent", "e3", turnEvents("e1")); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name   string
		child  func(t *testing.T, h *Harness) // the child's first attempt, e2
		source func(t *testing.T, h *Harness) // what happens to the parent before the next attempt
		uid    string                         // the session of the next attempt
		exec   string
		hist   []api.Event
		want   []string // session flags on success; nil wants errStateLost
	}{
		{
			name: "cancelled child, parent unchanged", child: cancelled,
			uid: "child", exec: "e2", hist: forked,
			want: []string{"--resume", sessionID("parent", 0), "--fork-session", "--session-id", sessionID("child", 0)},
		},
		{name: "cancelled child, parent ran on", child: cancelled, source: parentRanOn, uid: "child", exec: "e2", hist: forked},
		{
			name: "cancelled child, parent turn in flight", child: cancelled,
			source: func(_ *testing.T, h *Harness) { setRunning(h, "parent", true) },
			uid:    "child", exec: "e2", hist: forked,
		},
		{
			name: "child failed before init, parent unchanged", child: failedBeforeInit,
			uid: "child", exec: "e5", hist: slices.Concat(forked, turnEvents("e2")),
			want: []string{"--resume", sessionID("parent", 0), "--fork-session", "--session-id", sessionID("child", 1)},
		},
		{
			name: "child failed before init, parent ran on", child: failedBeforeInit, source: parentRanOn,
			uid: "child", exec: "e5", hist: slices.Concat(forked, turnEvents("e2")),
		},
		// The grandchild forks from the child, which is still forking from the parent: the
		// parent's transcript, past the child's fork point, is not the child's conversation.
		{
			name: "fork of an unconfirmed child whose parent ran on", child: failedBeforeInit, source: parentRanOn,
			uid: "grandchild", exec: "e4", hist: slices.Concat(forked, turnEvents("e2"), forkMarker()),
		},
		{
			name: "confirmed child, parent ran on",
			child: func(t *testing.T, h *Harness) {
				if err := runTurn(t, h, "child", "e2", forked); err != nil {
					t.Fatal(err)
				}
			},
			source: parentRanOn,
			uid:    "child", exec: "e4", hist: slices.Concat(forked, turnEvents("e2")),
			want: []string{"--resume", sessionID("child", 0)},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
			if err := runTurn(t, h, "parent", "e1"); err != nil {
				t.Fatal(err)
			}
			test.child(t, h)
			if test.source != nil {
				test.source(t, h)
			}
			spawned := len(starts(t, h))
			err := runTurn(t, h, test.uid, test.exec, test.hist)
			if test.want != nil {
				if err != nil {
					t.Fatalf("attempt = %v", err)
				}
				wantFlags(t, h, test.want...)
				return
			}
			if !errors.Is(err, errStateLost) {
				t.Fatalf("attempt = %v, want %v", err, errStateLost)
			}
			if got := len(starts(t, h)); got != spawned {
				t.Fatalf("spawned %d processes, want %d", got, spawned)
			}
		})
	}
}

// TestForkChildReservesSource: while a fork child's attempt runs, before Claude Code has read the
// transcript it forks, a turn of the session it forks from is refused, so that transcript cannot
// move past the fork point. The refused turn is not recorded as started: a fork taken at it fails
// closed, and its retry runs once the child's attempt ends.
func TestForkChildReservesSource(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := runTurn(t, h, "parent", "e1"); err != nil {
		t.Fatal(err)
	}
	stop := pausedTurn(t, h, "child", "e2", slices.Concat(turnEvents("e1"), forkMarker()))
	if got := forkReaders(h, "parent"); got != 1 {
		t.Fatalf("parent reserved by %d attempts, want 1", got)
	}
	forkInFlightTurn(t, h, "parent", "e3", turnEvents("e1"))
	if err := stop(); err == nil {
		t.Fatal("cancelled child turn returned nil")
	}
	if got := forkReaders(h, "parent"); got != 0 {
		t.Fatalf("parent reserved by %d attempts after the child ended, want 0", got)
	}

	if err := runTurn(t, h, "child2", "e4", turnEvents("e1"), turnEvents("e3"), forkMarker()); !errors.Is(err, errStateLost) {
		t.Fatalf("fork at the refused turn = %v, want %v", err, errStateLost)
	}
	if err := runTurn(t, h, "parent", "e3", turnEvents("e1")); err != nil {
		t.Fatalf("retry of the refused parent turn = %v", err)
	}
	wantFlags(t, h, "--resume", sessionID("parent", 0))
}

// TestForkChildRetryReservesSource: each attempt of a fork child not yet confirmed reserves the
// session it forks from again, and releases it when it ends.
func TestForkChildRetryReservesSource(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := runTurn(t, h, "parent", "e1"); err != nil {
		t.Fatal(err)
	}
	forked := slices.Concat(turnEvents("e1"), forkMarker())
	setScenario(t, h, scenario{Exit: 1})
	if err := runTurn(t, h, "child", "e2", forked); err == nil {
		t.Fatal("failed child turn returned nil")
	}
	if got := forkReaders(h, "parent"); got != 0 {
		t.Fatalf("parent reserved by %d attempts after the first ended, want 0", got)
	}

	stop := pausedTurn(t, h, "child", "e3", slices.Concat(forked, turnEvents("e2")))
	wantFlags(t, h, "--resume", sessionID("parent", 0), "--fork-session", "--session-id", sessionID("child", 1))
	forkInFlightTurn(t, h, "parent", "e4", turnEvents("e1"))
	if err := stop(); err == nil {
		t.Fatal("cancelled child turn returned nil")
	}
	if err := runTurn(t, h, "parent", "e4", turnEvents("e1")); err != nil {
		t.Fatalf("parent turn after the retry ended = %v", err)
	}
}

// TestForkChildReleasesSource: every way a fork child's attempt ends releases the session it
// forks from; an attempt refused before it is recorded as started reserves nothing.
func TestForkChildReleasesSource(t *testing.T) {
	t.Parallel()
	forked := slices.Concat(turnEvents("e1"), forkMarker())
	for _, test := range []struct {
		name  string
		child func(t *testing.T, h *Harness)
	}{
		{name: "ctx done before begin", child: func(t *testing.T, h *Harness) { refusedTurn(t, h, "child", "e2", forked) }},
		{name: "input validation fails", child: func(t *testing.T, h *Harness) {
			data := api.Message{Role: "user", Parts: []api.Part{{Data: map[string]any{"k": "v"}}}}
			s := &api.Start{ExecutionID: "e2", SessionUID: "child", History: forked, Inputs: []api.Message{data}}
			if err := h.Run(context.Background(), s, &recordingSink{t: t}); !errors.Is(err, errNonText) {
				t.Fatalf("child turn = %v, want %v", err, errNonText)
			}
		}},
		{name: "fails before init", child: func(t *testing.T, h *Harness) {
			setScenario(t, h, scenario{Exit: 1})
			if err := runTurn(t, h, "child", "e2", forked); err == nil {
				t.Fatal("failed child turn returned nil")
			}
			setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
		}},
		{name: "cancelled while running", child: func(t *testing.T, h *Harness) {
			if err := pausedTurn(t, h, "child", "e2", forked)(); err == nil {
				t.Fatal("cancelled child turn returned nil")
			}
		}},
		{name: "succeeds", child: func(t *testing.T, h *Harness) {
			if err := runTurn(t, h, "child", "e2", forked); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
			if err := runTurn(t, h, "parent", "e1"); err != nil {
				t.Fatal(err)
			}
			test.child(t, h)
			if got := forkReaders(h, "parent"); got != 0 {
				t.Fatalf("parent reserved by %d attempts, want 0", got)
			}
			if err := runTurn(t, h, "parent", "e3", turnEvents("e1")); err != nil {
				t.Fatalf("parent turn = %v", err)
			}
		})
	}
}

// TestForkOfUnconfirmedForkReservesChain: a grandchild forking through a child whose id is not
// confirmed reserves both, so neither the parent, whose transcript it copies, nor the child can
// start a turn until the grandchild's attempt ends.
func TestForkOfUnconfirmedForkReservesChain(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := runTurn(t, h, "parent", "e1"); err != nil {
		t.Fatal(err)
	}
	forked := slices.Concat(turnEvents("e1"), forkMarker())
	setScenario(t, h, scenario{Exit: 1})
	if err := runTurn(t, h, "child", "e2", forked); err == nil {
		t.Fatal("failed child turn returned nil")
	}
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	childHist := slices.Concat(forked, turnEvents("e2"))

	stop := pausedTurn(t, h, "grandchild", "e3", slices.Concat(childHist, forkMarker()))
	wantFlags(t, h, "--resume", sessionID("parent", 0), "--fork-session", "--session-id", sessionID("grandchild", 0))
	forkInFlightTurn(t, h, "parent", "e4", turnEvents("e1"))
	forkInFlightTurn(t, h, "child", "e5", childHist)
	if err := stop(); err == nil {
		t.Fatal("cancelled grandchild turn returned nil")
	}
	for _, uid := range []string{"parent", "child"} {
		if got := forkReaders(h, uid); got != 0 {
			t.Fatalf("%s reserved by %d attempts, want 0", uid, got)
		}
	}
	if err := runTurn(t, h, "child", "e5", childHist); err != nil {
		t.Fatalf("child turn = %v", err)
	}
	if err := runTurn(t, h, "parent", "e4", turnEvents("e1")); err != nil {
		t.Fatalf("parent turn = %v", err)
	}
}

// TestConcurrentForksReserveSource: two fork children in flight each hold the parent; it stays
// reserved until both attempts end.
func TestConcurrentForksReserveSource(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := runTurn(t, h, "parent", "e1"); err != nil {
		t.Fatal(err)
	}
	forked := slices.Concat(turnEvents("e1"), forkMarker())
	stop1 := pausedTurn(t, h, "child1", "e2", forked)
	stop2 := pausedTurn(t, h, "child2", "e3", forked)
	if got := forkReaders(h, "parent"); got != 2 {
		t.Fatalf("parent reserved by %d attempts, want 2", got)
	}
	if err := stop1(); err == nil {
		t.Fatal("cancelled child1 turn returned nil")
	}
	forkInFlightTurn(t, h, "parent", "e4", turnEvents("e1"))
	if err := stop2(); err == nil {
		t.Fatal("cancelled child2 turn returned nil")
	}
	if err := runTurn(t, h, "parent", "e4", turnEvents("e1")); err != nil {
		t.Fatalf("parent turn = %v", err)
	}
}

// TestConfirmedForkReservesNothing: once Claude Code confirmed a fork child's own id, its turns
// resume its own transcript, so they leave the parent free to run.
func TestConfirmedForkReservesNothing(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl"})
	if err := runTurn(t, h, "parent", "e1"); err != nil {
		t.Fatal(err)
	}
	forked := slices.Concat(turnEvents("e1"), forkMarker())
	if err := runTurn(t, h, "child", "e2", forked); err != nil {
		t.Fatal(err)
	}
	stop := pausedTurn(t, h, "child", "e3", slices.Concat(forked, turnEvents("e2")))
	wantFlags(t, h, "--resume", sessionID("child", 0))
	if got := forkReaders(h, "parent"); got != 0 {
		t.Fatalf("parent reserved by %d attempts, want 0", got)
	}
	if err := runTurn(t, h, "parent", "e4", turnEvents("e1")); err != nil {
		t.Fatalf("parent turn = %v", err)
	}
	if err := stop(); err == nil {
		t.Fatal("cancelled child turn returned nil")
	}
}
