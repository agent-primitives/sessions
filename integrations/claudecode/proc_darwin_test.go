//go:build darwin

package claudecode

import "golang.org/x/sys/unix"

// pStatZombie is SZOMB, the p_stat of a process that exited and was not yet reaped.
const pStatZombie = 5

// isZombie reports whether pid is a process that exited and was not yet reaped.
func isZombie(pid int) (bool, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return false, err
	}
	return kp.Proc.P_stat == pStatZombie, nil
}
