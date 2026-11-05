//go:build linux

package claudecode

import (
	"bytes"
	"fmt"
	"os"
)

// isZombie reports whether pid is a process that exited and was not yet reaped.
func isZombie(pid int) (bool, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false, err
	}
	// The state follows the parenthesized command name, which may itself contain ") ".
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+2 >= len(b) {
		return false, fmt.Errorf("malformed /proc/%d/stat: %q", pid, b)
	}
	return b[i+2] == 'Z', nil
}
