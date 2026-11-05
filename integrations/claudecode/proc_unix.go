//go:build unix

package claudecode

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// sysProcAttr puts the process in its own process group, so the harness can kill everything it
// started.
func sysProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

// interruptProcess sends SIGINT to the process itself, which Claude Code handles by ending the
// turn. os.Process refuses to signal a process that was already waited for.
func interruptProcess(p *os.Process) { _ = p.Signal(os.Interrupt) }

// killGroup kills the process group the process leads (sysProcAttr sets Setpgid). The kernel does
// not reuse a pid while a process group with that id has members, so -pid reaches only this group
// while any member lives. A descendant that left the group (setsid) is not reached.
func killGroup(p *os.Process) { _ = syscall.Kill(-p.Pid, syscall.SIGKILL) }

// stopGroup kills the process group the process led and waits up to wait until the group has no
// members, a process that is still exiting or not yet reaped included. It reports whether the
// group emptied. The leader must already have been waited for.
func stopGroup(p *os.Process, wait time.Duration) bool {
	killGroup(p)
	deadline := time.Now().Add(wait)
	for !errors.Is(syscall.Kill(-p.Pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}
