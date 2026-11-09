//go:build !linux && !darwin

package claudecode

import (
	"errors"
	"os"
)

// waitExit is not implemented here: other platforms may have a wait that leaves the process
// unreaped (wait6 WNOWAIT, kqueue NOTE_EXIT), but none is used or tested here. The turn falls back
// to cmd.Wait, which reaps the leader as soon as it exits, so a later kill of its group signals an
// id nothing reserves once every member has exited.
func waitExit(*os.Process) error { return errors.ErrUnsupported }
