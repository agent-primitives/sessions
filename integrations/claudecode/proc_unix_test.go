//go:build unix

package claudecode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// runAsync runs one turn in the background and returns its result channel.
func runAsync(ctx context.Context, h *Harness, exec string, sink *recordingSink) <-chan error {
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx, startFor("uid", exec, nil, "go"), sink) }()
	return done
}

func waitRun(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
		return nil
	}
}

func interrupted(t *testing.T, h *Harness) bool {
	t.Helper()
	return slices.ContainsFunc(records(t, h), func(r record) bool { return r.Event == "interrupt" })
}

func TestCancelSendsSIGINT(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "init_only.jsonl", Block: true})
	ctx, cancel := context.WithCancel(context.Background())
	sink := &recordingSink{t: t}
	done := runAsync(ctx, h, "e1", sink)
	waitRecord(t, h, "blocking")
	cancel()
	err := waitRun(t, done)
	if !errors.Is(err, context.Canceled) || err.Error() != "claudecode: turn interrupted: context canceled" {
		t.Fatalf("Run = %v", err)
	}
	if !interrupted(t, h) {
		t.Fatal("claude was not sent SIGINT")
	}
	if len(sink.outputs) != 0 || len(sink.usages) != 0 {
		t.Fatalf("cancelled turn recorded %q %+v", sink.outputs, sink.usages)
	}
}

func TestCancelKillsGroupAfterGrace(t *testing.T) {
	t.Parallel()
	const grace = 300 * time.Millisecond
	h := newTestHarness(t, Config{InterruptGrace: grace})
	setScenario(t, h, scenario{Fixture: "init_only.jsonl", Block: true, OnInterrupt: "ignore"})
	ctx, cancel := context.WithCancel(context.Background())
	done := runAsync(ctx, h, "e1", &recordingSink{t: t})
	waitRecord(t, h, "blocking")
	start := time.Now()
	cancel()
	if err := waitRun(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if elapsed := time.Since(start); elapsed < grace {
		t.Fatalf("killed after %v, before the %v grace", elapsed, grace)
	}
	if !interrupted(t, h) {
		t.Fatal("claude was not sent SIGINT before the kill")
	}
}

// TestLingerAfterResult: a process that keeps running after its result is sent SIGINT after the
// post-result grace, and SIGKILL once the interrupt grace has passed. The turn still succeeds when
// it exits 0 (as Claude Code 2.1.311 does) or dies of a signal the harness delivered to it. A
// nonzero exit status is the process's own verdict even though SIGINT reached it: a process that
// exited on its own but was not yet reaped takes the signal the same way. So is a death by a
// signal the harness did not send.
func TestLingerAfterResult(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		sc     scenario
		killed bool   // the harness kills the process; otherwise it ends before the interrupt grace
		want   string // the error; "" for success
	}{
		{
			name: "exits 0 on SIGINT",
			sc:   scenario{Fixture: "text_turn.jsonl", Block: true},
		},
		{
			name: "dies of SIGINT",
			sc:   scenario{Fixture: "text_turn.jsonl", Block: true, OnInterrupt: "die"},
		},
		{
			name:   "ignores SIGINT until killed",
			sc:     scenario{Fixture: "text_turn.jsonl", Block: true, OnInterrupt: "ignore"},
			killed: true,
		},
		{
			name: "exits nonzero on SIGINT",
			sc:   scenario{Fixture: "text_turn.jsonl", Block: true, InterruptExit: 2},
			want: "claudecode: claude exited with status 2 after a success result",
		},
		{
			name: "dies of an outside SIGKILL after SIGINT",
			sc:   scenario{Fixture: "text_turn.jsonl", Block: true, OnInterrupt: "sigkill"},
			want: "claudecode: claude exited with status -1 after a success result",
		},
		{
			name: "dies of SIGTERM after SIGINT",
			sc:   scenario{Fixture: "text_turn.jsonl", Block: true, OnInterrupt: "sigterm"},
			want: "claudecode: claude exited with status -1 after a success result",
		},
		{
			name: "dies of SIGINT before a result",
			sc:   scenario{Fixture: "init_only.jsonl", CloseStdout: true, Block: true, OnInterrupt: "die"},
			want: errNoResult.Error(),
		},
		{
			name: "exits nonzero on SIGINT before a result",
			sc:   scenario{Fixture: "init_only.jsonl", CloseStdout: true, Block: true, InterruptExit: 3},
			want: "claudecode: claude exited with status 3 before a result",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// A process that ends by itself gets far longer than it needs, so it is never
			// killed: under -race a test binary's exit 0 takes about a second.
			grace := 10 * time.Second
			if test.killed {
				grace = 200 * time.Millisecond
			}
			h := newTestHarness(t, Config{InterruptGrace: grace})
			h.postResultGrace = 200 * time.Millisecond
			setScenario(t, h, test.sc)
			sink := &recordingSink{t: t}
			start := time.Now()
			err := waitRun(t, runAsync(context.Background(), h, "e1", sink))
			if elapsed := time.Since(start); !test.killed && elapsed >= grace {
				t.Fatalf("Run took %v, so the harness killed the process", elapsed)
			}
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(sink.outputs, []string{"Hello, world."}) {
					t.Fatalf("outputs = %q", sink.outputs)
				}
			} else {
				if err == nil || err.Error() != test.want {
					t.Fatalf("Run = %v, want %q", err, test.want)
				}
				if len(sink.outputs) != 0 {
					t.Fatalf("failed turn recorded output %q", sink.outputs)
				}
			}
			if !interrupted(t, h) {
				t.Fatal("lingering claude was not sent SIGINT")
			}
		})
	}
}

// TestOutsideKillWhenGraceFires: a process killed from outside after its success result, whose
// exit the turn has not yet seen when the post-result grace fires, takes the harness's SIGINT. It
// died of a SIGKILL the harness never sent, so the turn fails.
func TestOutsideKillWhenGraceFires(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	if h.postResultGrace == h.cfg.InterruptGrace {
		t.Fatal("the post-result and interrupt graces must differ to tell their deadlines apart")
	}
	var (
		armed       = make(chan struct{})
		fire        = make(chan time.Time)
		interrupted = make(chan struct{})
	)
	h.after = func(d time.Duration) <-chan time.Time {
		switch d {
		case h.postResultGrace:
			close(armed)
			return fire
		case h.cfg.InterruptGrace:
			close(interrupted)
			return nil // the harness never kills the process
		}
		return time.After(d)
	}
	// The turn cannot see the process exit until this returns.
	h.waitExit = func(p *os.Process) error {
		wait := func(what string, c <-chan struct{}) bool {
			select {
			case <-c:
				return true
			case <-time.After(10 * time.Second):
				t.Errorf("the turn never %s", what)
				return false
			}
		}
		if wait("armed the post-result grace", armed) {
			if err := p.Signal(os.Kill); err != nil {
				t.Errorf("outside SIGKILL = %v", err)
			}
			select {
			case fire <- time.Now():
				wait("sent SIGINT", interrupted)
			case <-time.After(10 * time.Second):
				t.Error("the turn never waited on the post-result grace")
			}
		}
		return waitExit(p)
	}
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl", Block: true, OnInterrupt: "ignore"})
	sink := &recordingSink{t: t}
	err := waitRun(t, runAsync(context.Background(), h, "e1", sink))
	if want := "claudecode: claude exited with status -1 after a success result"; err == nil || err.Error() != want {
		t.Fatalf("Run = %v, want %q", err, want)
	}
	if len(sink.outputs) != 0 {
		t.Fatalf("failed turn recorded output %q", sink.outputs)
	}
}

// TestExitWhenGraceFires: a process that ends on its own as the post-result grace fires, before the
// turn saw the exit, still fails the turn. The harness interrupts a process whose exit was already
// observed: an unreaped zombie takes the SIGINT, but it does not count, so it does not excuse the
// exit, even one by SIGINT.
func TestExitWhenGraceFires(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		sc   scenario
		want string
	}{
		{
			name: "after a success result",
			sc:   scenario{Fixture: "text_turn.jsonl", Exit: 2},
			want: "claudecode: claude exited with status 2 after a success result",
		},
		{
			name: "died of SIGINT after a success result",
			sc:   scenario{Fixture: "text_turn.jsonl", SelfSIGINT: true},
			want: "claudecode: claude exited with status -1 after a success result",
		},
		{
			name: "stdout closed without a result",
			sc:   scenario{Fixture: "init_only.jsonl", Exit: 3},
			want: "claudecode: claude exited with status 3 before a result",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			if h.postResultGrace == h.cfg.InterruptGrace {
				t.Fatal("the post-result and interrupt graces must differ to tell their deadlines apart")
			}
			fire := make(chan time.Time)
			h.after = func(d time.Duration) <-chan time.Time {
				if d == h.postResultGrace {
					return fire
				}
				return time.After(d)
			}
			// The leader's exit was observed, but the turn sees it only after the grace fired.
			h.exitObserved = func() {
				select {
				case fire <- time.Now():
				case <-time.After(10 * time.Second):
					t.Error("the turn never waited on the post-result grace")
				}
			}
			setScenario(t, h, test.sc)
			sink := &recordingSink{t: t}
			err := waitRun(t, runAsync(context.Background(), h, "e1", sink))
			if err == nil || err.Error() != test.want {
				t.Fatalf("Run = %v, want %q", err, test.want)
			}
			if len(sink.outputs) != 0 {
				t.Fatalf("failed turn recorded output %q", sink.outputs)
			}
			if interrupted(t, h) {
				t.Fatal("claude recorded a SIGINT")
			}
		})
	}
}

// TestInterruptProcess: SIGINT reaches a live process; a process already waited for refuses it.
func TestInterruptProcess(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := interruptProcess(cmd.Process); err != nil {
		t.Fatalf("interrupt a live process = %v", err)
	}
	_ = cmd.Wait()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Fatalf("process state = %v, want killed by SIGINT", cmd.ProcessState)
	}
	if err := interruptProcess(cmd.Process); !errors.Is(err, os.ErrProcessDone) {
		t.Fatalf("interrupt a waited-for process = %v, want %v", err, os.ErrProcessDone)
	}
}

// TestKillGroup: killGroup reports SIGKILL delivered to a live leader and refused by one already
// waited for, which stoppedByHarness then does not count.
func TestKillGroup(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = sysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if !killGroup(cmd.Process) {
		t.Fatal("killGroup of a live process reported no delivery")
	}
	_ = cmd.Wait()
	if !stoppedByHarness(cmd.ProcessState, false, true) {
		t.Fatalf("process state %v with the harness's SIGKILL is not stopped by the harness", cmd.ProcessState)
	}
	if stoppedByHarness(cmd.ProcessState, true, false) {
		t.Fatalf("process state %v without the harness's SIGKILL is stopped by the harness", cmd.ProcessState)
	}
	if killGroup(cmd.Process) {
		t.Fatal("killGroup of a waited-for process reported delivery")
	}
	if stoppedByHarness(nil, true, true) {
		t.Fatal("a nil process state is stopped by the harness")
	}
}

// TestGrandchildHoldingStdout: a child claude left behind holding stdout is killed with the group
// once the leader exits, rather than blocking the turn.
func TestGrandchildHoldingStdout(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	h.drainGrace = 200 * time.Millisecond
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl", Grandchild: true})
	sink := &recordingSink{t: t}
	if err := waitRun(t, runAsync(context.Background(), h, "e1", sink)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sink.outputs, []string{"Hello, world."}) {
		t.Fatalf("outputs = %q", sink.outputs)
	}
	var pid int
	for _, r := range records(t, h) {
		if r.Event == "grandchild" {
			pid = r.PID
		}
	}
	if pid == 0 {
		t.Fatal("fake claude recorded no grandchild")
	}
	deadline := time.Now().Add(10 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still running", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestConcurrentTurnRefused(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	setScenario(t, h, scenario{Fixture: "init_only.jsonl", Block: true})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runAsync(ctx, h, "e1", &recordingSink{t: t})
	waitRecord(t, h, "blocking")
	if err := h.Run(context.Background(), startFor("uid", "e2", nil, "go"), &recordingSink{t: t}); !errors.Is(err, errBusy) {
		t.Fatalf("concurrent Run = %v, want %v", err, errBusy)
	}
	cancel()
	waitRun(t, done)
	if got := starts(t, h); len(got) != 1 {
		t.Fatalf("spawned %d processes, want 1", len(got))
	}
}

// TestExitStatusWhileStdoutHeld: the leader's own exit status still decides the turn when it exits
// before stdout closes, here because a child holds it. The harness kills the group once the drain
// grace passes; the unreaped leader takes that SIGKILL too, but it came after the exit, so it does
// not excuse a death by SIGKILL.
func TestExitStatusWhileStdoutHeld(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		sc   scenario
		want string
	}{
		{
			name: "exits nonzero",
			sc:   scenario{Fixture: "text_turn.jsonl", Grandchild: true, Exit: 2},
			want: "claudecode: claude exited with status 2 after a success result",
		},
		{
			name: "dies of an outside SIGKILL",
			sc:   scenario{Fixture: "text_turn.jsonl", Grandchild: true, SelfSIGKILL: true},
			want: "claudecode: claude exited with status -1 after a success result",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			h.drainGrace = 200 * time.Millisecond
			setScenario(t, h, test.sc)
			sink := &recordingSink{t: t}
			err := waitRun(t, runAsync(context.Background(), h, "e1", sink))
			if err == nil || err.Error() != test.want {
				t.Fatalf("Run = %v, want %q", err, test.want)
			}
			if len(sink.outputs) != 0 {
				t.Fatalf("outputs = %q", sink.outputs)
			}
		})
	}
}

// TestWaitExitFails: when the platform cannot wait for the exit without reaping it, the turn reaps
// the leader at once, as before, and still kills its group and reads its exit status. A real
// failure is logged once per harness; ErrUnsupported is not logged.
func TestWaitExitFails(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		err   error
		warns int
	}{
		{name: "error", err: syscall.EINVAL, warns: 1},
		{name: "unsupported", err: errors.ErrUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			var logs bytes.Buffer
			h.logger = slog.New(slog.NewTextHandler(&logs, nil))
			h.waitExit = func(*os.Process) error { return test.err }
			for _, uid := range []string{"uid1", "uid2"} {
				setScenario(t, h, scenario{Fixture: "text_turn.jsonl", Detached: true})
				sink := &recordingSink{t: t}
				if err := h.Run(context.Background(), startFor(uid, "e-"+uid, nil, "go"), sink); err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(sink.outputs, []string{"Hello, world."}) {
					t.Fatalf("outputs = %q", sink.outputs)
				}
			}
			for _, r := range records(t, h) {
				if r.Event == "detached" {
					t.Cleanup(func() { _ = syscall.Kill(r.PID, syscall.SIGKILL) })
					waitGone(t, r.PID)
				}
			}
			if got := strings.Count(logs.String(), "cannot wait for claude's exit"); got != test.warns {
				t.Fatalf("warned %d times, want %d: %s", got, test.warns, logs.String())
			}
		})
	}
}

// recordedPID returns the pid the fake recorded for event, killing it when the test ends in case
// the harness left it running.
func recordedPID(t *testing.T, h *Harness, event string) int {
	t.Helper()
	for _, r := range records(t, h) {
		if r.Event == event && r.PID > 0 {
			t.Cleanup(func() { _ = syscall.Kill(r.PID, syscall.SIGKILL) })
			return r.PID
		}
	}
	t.Fatalf("fake claude recorded no %s child", event)
	return 0
}

// waitGone waits until pid no longer exists. The harness has killed it by the time Run returns;
// the wait covers only its reaping by init.
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("child %d still running after Run returned", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestGroupKilledWhenRunReturns: a tool Claude Code started that redirected its stdio holds no
// stdout, so the turn ends on the leader's exit alone, cancelled or successful. The child is in the
// turn's process group and must not outlive Run.
func TestGroupKilledWhenRunReturns(t *testing.T) {
	t.Parallel()
	t.Run("cancelled", func(t *testing.T) {
		t.Parallel()
		h := newTestHarness(t, Config{})
		setScenario(t, h, scenario{Fixture: "init_only.jsonl", Block: true, Detached: true})
		ctx, cancel := context.WithCancel(context.Background())
		done := runAsync(ctx, h, "e1", &recordingSink{t: t})
		waitRecord(t, h, "blocking")
		pid := recordedPID(t, h, "detached")
		cancel()
		if err := waitRun(t, done); !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v", err)
		}
		waitGone(t, pid)
	})
	t.Run("successful", func(t *testing.T) {
		t.Parallel()
		h := newTestHarness(t, Config{})
		setScenario(t, h, scenario{Fixture: "text_turn.jsonl", Detached: true})
		sink := &recordingSink{t: t}
		if err := waitRun(t, runAsync(context.Background(), h, "e1", sink)); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(sink.outputs, []string{"Hello, world."}) {
			t.Fatalf("outputs = %q", sink.outputs)
		}
		waitGone(t, recordedPID(t, h, "detached"))
	})
}

// TestSlowSinkAfterExit: lines the process wrote before it exited are read even when the sink holds
// each Report for longer than drainGrace. The drain grace bounds how long stdout stays open while
// the harness waits on it, not how long the host takes to record a line.
func TestSlowSinkAfterExit(t *testing.T) {
	t.Parallel()
	const tools = 12
	lines := []string{`{"type":"system","subtype":"init","session_id":"{{SESSION_ID}}","model":"claude-test-model-1"}`}
	for i := range tools {
		id := fmt.Sprintf("toolu_%02d", i)
		lines = append(lines,
			`{"type":"assistant","message":{"id":"msg_`+id+`","content":[{"type":"tool_use","id":"`+id+`","name":"Bash","input":{}}]},"session_id":"{{SESSION_ID}}"}`,
			`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"`+id+`","content":"`+strings.Repeat("x", 20_000)+`"}]},"session_id":"{{SESSION_ID}}"}`)
	}
	lines = append(lines, `{"type":"result","subtype":"success","result":"Done.","session_id":"{{SESSION_ID}}"}`)
	h := newTestHarness(t, Config{})
	h.drainGrace = 50 * time.Millisecond
	setScenario(t, h, scenario{Lines: lines})
	// The results overflow the pipe and the reader's buffer, so the fake exits with lines still in
	// the pipe, and each Report outlasts drainGrace.
	sink := &recordingSink{t: t, reportDelay: 150 * time.Millisecond}
	if err := waitRun(t, runAsync(context.Background(), h, "e1", sink)); err != nil {
		t.Fatalf("Run = %v (%d reports)", err, len(sink.reports))
	}
	if len(sink.reports) != tools || !slices.Equal(sink.outputs, []string{"Done."}) {
		t.Fatalf("reports = %d, outputs = %q", len(sink.reports), sink.outputs)
	}
}

// TestDrainBoundedWhileStdoutTrickles: a child that keeps writing to stdout after the leader exits
// does not keep the turn open past drainGrace of waiting, however often it writes.
func TestDrainBoundedWhileStdoutTrickles(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	h.drainGrace = 200 * time.Millisecond
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl", Trickle: true})
	sink := &recordingSink{t: t}
	if err := waitRun(t, runAsync(context.Background(), h, "e1", sink)); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(sink.outputs, []string{"Hello, world."}) {
		t.Fatalf("outputs = %q", sink.outputs)
	}
	waitGone(t, recordedPID(t, h, "trickle"))
}
