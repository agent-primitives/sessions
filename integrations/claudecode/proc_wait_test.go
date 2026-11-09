//go:build linux || darwin

package claudecode

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestWaitExitLeavesZombie: waitExit returns once the process exited, whether it is still running
// when called or already exited, and leaves it unreaped: the zombie keeps the id of the group it
// leads, and cmd.Wait still reports its exit status.
func TestWaitExitLeavesZombie(t *testing.T) {
	t.Parallel()
	for _, running := range []bool{true, false} {
		name := "already exited"
		if running {
			name = "running"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cmd := exec.Command("sh", "-c", "read x; exit 3")
			cmd.SysProcAttr = sysProcAttr()
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			pid := cmd.Process.Pid
			t.Cleanup(func() {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			})
			done := make(chan error, 1)
			if running {
				go func() { done <- waitExit(cmd.Process) }()
				select {
				case err := <-done:
					t.Fatalf("waitExit returned %v while the process runs", err)
				case <-time.After(100 * time.Millisecond):
				}
				_ = stdin.Close()
			} else {
				_ = stdin.Close()
				waitZombie(t, pid)
				done <- waitExit(cmd.Process)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("waitExit = %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("waitExit did not return after the exit")
			}
			if z, err := isZombie(pid); err != nil || !z {
				t.Fatalf("after waitExit: zombie = %v, %v; want an unreaped process", z, err)
			}
			// linux takes a signal to a zombie-only group; darwin refuses it with EPERM. Either
			// way the group id still exists.
			if err := syscall.Kill(-pid, syscall.SIGKILL); errors.Is(err, syscall.ESRCH) {
				t.Fatalf("kill(-%d) = %v: the group id was released before the reap", pid, err)
			}
			if err := cmd.Wait(); cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != 3 {
				t.Fatalf("cmd.Wait = %v, state %v; want exit status 3", err, cmd.ProcessState)
			}
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("kill(%d, 0) after the reap = %v, want ESRCH", pid, err)
			}
		})
	}
}

// waitZombie waits until pid is an unreaped zombie.
func waitZombie(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		z, err := isZombie(pid)
		if err != nil {
			t.Fatal(err)
		}
		if z {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d never exited", pid)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// goneOrZombie reports whether pid no longer runs: it was reaped, or it is a zombie its new parent
// has not reaped yet.
func goneOrZombie(pid int) bool {
	if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		return true
	}
	z, err := isZombie(pid)
	return err != nil || z
}

// TestLeaderUnreapedWhileGroupStops: the turn reaps the leader only after the last kill of its
// group, so every kill reaches the group while the zombie leader still reserves its id: the final
// kill on every path, and the kill after the drain grace when a child holds stdout. The leader is
// reaped by the time Run returns.
func TestLeaderUnreapedWhileGroupStops(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		sc     scenario
		member string // the fake's child that stays in the group
		cancel bool
		want   error
	}{
		{name: "successful", sc: scenario{Fixture: "text_turn.jsonl", Detached: true}, member: "detached"},
		{
			name:   "cancelled",
			sc:     scenario{Fixture: "init_only.jsonl", Block: true, Detached: true},
			member: "detached",
			cancel: true,
			want:   context.Canceled,
		},
		{name: "child holds stdout", sc: scenario{Fixture: "text_turn.jsonl", Grandchild: true}, member: "grandchild"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			h := newTestHarness(t, Config{})
			h.drainGrace = 200 * time.Millisecond
			leader := make(chan int, 1)
			h.beforeReap = func(pid int) {
				leader <- pid
				if z, err := isZombie(pid); err != nil || !z {
					t.Errorf("before the reap: leader zombie = %v, %v; want unreaped", z, err)
				}
				if err := syscall.Kill(-pid, 0); errors.Is(err, syscall.ESRCH) {
					t.Errorf("before the reap: kill(-%d, 0) = %v; the group id was released", pid, err)
				}
				// Not on the test goroutine: report failures with t.Error only.
				var member int
				b, _ := os.ReadFile(filepath.Join(h.cfg.WorkDir, recordFile))
				for _, line := range strings.Split(string(b), "\n") {
					var r record
					if json.Unmarshal([]byte(line), &r) == nil && r.Event == test.member && r.PID > 0 {
						member = r.PID
						t.Cleanup(func() { _ = syscall.Kill(member, syscall.SIGKILL) })
					}
				}
				if member == 0 {
					t.Errorf("fake claude recorded no %s child", test.member)
					return
				}
				deadline := time.Now().Add(5 * time.Second)
				for !goneOrZombie(member) {
					if time.Now().After(deadline) {
						t.Errorf("before the reap: %s %d still runs after the group kill", test.member, member)
						return
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			setScenario(t, h, test.sc)
			ctx, cancel := context.WithCancel(context.Background())
			sink := &recordingSink{t: t}
			// beforeReap reports through t, so Run must return before the test completes, even
			// when the test fails first.
			var (
				result   = runAsync(ctx, h, "e1", sink)
				done     = make(chan error, 1)
				returned = make(chan struct{})
			)
			go func() {
				done <- <-result
				close(returned)
			}()
			t.Cleanup(func() {
				cancel()
				<-returned
			})
			if test.cancel {
				waitRecord(t, h, "blocking")
				cancel()
			}
			err := waitRun(t, done)
			if test.want != nil {
				if !errors.Is(err, test.want) {
					t.Fatalf("Run = %v, want %v", err, test.want)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(sink.outputs, []string{"Hello, world."}) {
					t.Fatalf("outputs = %q", sink.outputs)
				}
			}
			pid := <-leader
			if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
				t.Fatalf("after Run: kill(%d, 0) = %v, want ESRCH (reaped)", pid, err)
			}
		})
	}
}
