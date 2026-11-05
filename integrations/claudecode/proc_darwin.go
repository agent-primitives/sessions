//go:build darwin

package claudecode

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// waitExit blocks until p exits and leaves it unreaped (kqueue EVFILT_PROC NOTE_EXIT, which does
// not reap): until cmd.Wait reaps it, the zombie keeps its pid, and the id of the process group it
// leads, from being reused.
func waitExit(p *os.Process) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(kq) }()
	unix.CloseOnExec(kq)
	change := unix.Kevent_t{
		Ident:  uint64(p.Pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_ONESHOT,
		Fflags: unix.NOTE_EXIT,
	}
	// With no room for events, a registration error is returned, not reported as an event.
	if _, err := unix.Kevent(kq, []unix.Kevent_t{change}, nil, nil); err != nil {
		if errors.Is(err, unix.ESRCH) {
			return nil // already exited: an unreaped child of the turn is a zombie
		}
		return err
	}
	events := make([]unix.Kevent_t, 1)
	for {
		n, err := unix.Kevent(kq, nil, events, nil)
		switch {
		case errors.Is(err, unix.EINTR):
			continue
		case err != nil:
			return err
		case n == 0:
			continue
		case events[0].Flags&unix.EV_ERROR != 0:
			return unix.Errno(events[0].Data)
		}
		return nil
	}
}
