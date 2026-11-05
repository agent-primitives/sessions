//go:build !unix

package claudecode

import (
	"os"
	"syscall"
	"time"
)

// sysProcAttr sets nothing: there is no process group to create.
func sysProcAttr() *syscall.SysProcAttr { return nil }

// interruptProcess kills the process: there is no SIGINT to send.
func interruptProcess(p *os.Process) { _ = p.Kill() }

// killGroup kills the process; processes it started are not reached.
func killGroup(p *os.Process) { _ = p.Kill() }

// stopGroup has no group to stop: the process was already waited for, and processes it started
// are not reached.
func stopGroup(*os.Process, time.Duration) bool { return true }
