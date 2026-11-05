//go:build linux

package claudecode

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// waitExit blocks until p exits and leaves it unreaped (waitid WNOWAIT): until cmd.Wait reaps it,
// the zombie keeps its pid, and the id of the process group it leads, from being reused. Nothing
// but the turn reaps the process, so waiting on its pid is safe.
func waitExit(p *os.Process) error {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, p.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}
