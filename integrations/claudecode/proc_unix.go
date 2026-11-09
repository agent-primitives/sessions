//go:build unix

package claudecode

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// checkPlatform accepts this platform: it has process groups.
func checkPlatform() error { return nil }

// sysProcAttr puts the process in its own process group, so the harness can kill everything it
// started.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// interruptProcess sends SIGINT to the process itself, which Claude Code handles by ending the
// turn. A nil error means it was sent to a process not yet waited for, which may already have
// exited: an unreaped zombie takes the signal. os.Process refuses a process that was waited for
// (os.ErrProcessDone).
func interruptProcess(p *os.Process) error { return p.Signal(os.Interrupt) }

// stoppedByHarness reports whether the process died of a signal the harness delivered to it:
// SIGINT when interrupted, SIGKILL when killed. Claude Code 2.1.311 handles SIGINT and exits 0,
// which needs no excuse; any nonzero exit status, or a death by a signal the harness did not
// deliver, is the process's own verdict.
func stoppedByHarness(ps *os.ProcessState, interrupted, killed bool) bool {
	if ps == nil {
		return false
	}
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return false
	}
	switch ws.Signal() {
	case syscall.SIGINT:
		return interrupted
	case syscall.SIGKILL:
		return killed
	}
	return false
}

// killGroup sends SIGKILL to the process and then to the process group it leads (sysProcAttr sets
// Setpgid). It reports whether the process itself took the signal: one already waited for refuses
// it, while an unreaped zombie takes it. The kernel does not reuse a pid while that process is
// unreaped, nor while a process group with that id has members, so -pid reaches only this group
// while the leader is unreaped (waitExit leaves it so on linux and darwin) or any member lives. A
// descendant that left the group (setsid) is not reached.
func killGroup(p *os.Process) bool {
	delivered := p.Signal(os.Kill) == nil
	_ = syscall.Kill(-p.Pid, syscall.SIGKILL)
	return delivered
}

// stopGroup kills the process group the process leads, then calls reap, which must reap the
// process unless it was already waited for, and then waits up to wait until the group has no
// members, a process that is still exiting or not yet reaped included. It reports whether the group
// emptied. The kill comes before the reap, so it reaches the group while the unreaped leader still
// reserves its id. The wait must follow the reap: a zombie leader is still a member, so the group
// never empties before it. The wait only sends signal 0, so if the id is reused once the group has
// emptied, the worst case is a false report.
func stopGroup(p *os.Process, reap func(), wait time.Duration) bool {
	killGroup(p)
	reap()
	deadline := time.Now().Add(wait)
	for !errors.Is(syscall.Kill(-p.Pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}
