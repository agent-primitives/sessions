//go:build !unix

package claudecode

import (
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
)

// checkPlatform refuses this platform: the harness ends a turn's tool processes by killing the
// turn's process group, and this platform has no equivalent here (it would need something like a
// Windows Job Object). Without it a turn would report a clean teardown while a process a tool
// started kept running.
func checkPlatform() error {
	return fmt.Errorf("claudecode: unsupported platform %s: the harness needs Unix process groups "+
		"to end the processes a turn's tools start", runtime.GOOS)
}

// The functions below exist only so the package compiles here. New refuses this platform, so none
// of them runs.

func sysProcAttr() *syscall.SysProcAttr { return nil }

func interruptProcess(p *os.Process) error { return p.Kill() }

func stoppedByHarness(*os.ProcessState, bool, bool) bool { return false }

func killGroup(p *os.Process) bool { return p.Kill() == nil }

// stopGroup only reaps, and reports false: it never confirms a cleanup it cannot perform.
func stopGroup(_ *os.Process, reap func(), _ time.Duration) bool {
	reap()
	return false
}
