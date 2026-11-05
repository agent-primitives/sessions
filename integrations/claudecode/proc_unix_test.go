//go:build unix

package claudecode

import (
	"context"
	"errors"
	"slices"
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

// TestLingerAfterResult: a process that keeps running after its result is interrupted after the
// post-result grace, and the turn still succeeds.
func TestLingerAfterResult(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	h.postResultGrace = 200 * time.Millisecond
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl", Block: true})
	sink := &recordingSink{t: t}
	if err := waitRun(t, runAsync(context.Background(), h, "e1", sink)); err != nil {
		t.Fatal(err)
	}
	if !interrupted(t, h) {
		t.Fatal("lingering claude was not sent SIGINT")
	}
	if !slices.Equal(sink.outputs, []string{"Hello, world."}) {
		t.Fatalf("outputs = %q", sink.outputs)
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
// before stdout closes, here because a child holds it.
func TestExitStatusWhileStdoutHeld(t *testing.T) {
	t.Parallel()
	h := newTestHarness(t, Config{})
	h.drainGrace = 200 * time.Millisecond
	setScenario(t, h, scenario{Fixture: "text_turn.jsonl", Grandchild: true, Exit: 2})
	sink := &recordingSink{t: t}
	err := waitRun(t, runAsync(context.Background(), h, "e1", sink))
	if err == nil || err.Error() != "claudecode: claude exited with status 2 after a success result" {
		t.Fatalf("Run = %v", err)
	}
	if len(sink.outputs) != 0 {
		t.Fatalf("outputs = %q", sink.outputs)
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
