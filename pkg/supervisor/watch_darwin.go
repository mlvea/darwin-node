//go:build darwin

package supervisor

import "golang.org/x/sys/unix"

// watchFDs blocks until wakeR is readable or a watched connection dies.
// EV_CLEAR is edge trigger: a queued guest payload is reported once and
// left for the agent. EV_EOF is the peer closing, including SIGKILL of
// the supervisor.
func watchFDs(wakeR, ctrlFD, guestFD int) error {
	kq, err := unix.Kqueue()
	if err != nil {
		return err
	}
	unix.CloseOnExec(kq)
	defer unix.Close(kq)
	changes := []unix.Kevent_t{
		{Ident: uint64(wakeR), Filter: unix.EVFILT_READ, Flags: unix.EV_ADD},
		{Ident: uint64(ctrlFD), Filter: unix.EVFILT_READ, Flags: unix.EV_ADD | unix.EV_CLEAR},
		{Ident: uint64(guestFD), Filter: unix.EVFILT_READ, Flags: unix.EV_ADD | unix.EV_CLEAR},
	}
	if _, err := unix.Kevent(kq, changes, nil, nil); err != nil {
		return err
	}
	events := make([]unix.Kevent_t, 3)
	for {
		n, err := unix.Kevent(kq, nil, events, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		wake := false
		dead := false
		for i := 0; i < n; i++ {
			ev := events[i]
			if ev.Filter == unix.EVFILT_READ && int(ev.Ident) == wakeR {
				wake = true
				continue
			}
			if ev.Flags&unix.EV_ERROR != 0 || ev.Flags&unix.EV_EOF != 0 {
				dead = true
			}
		}
		if wake {
			return nil
		}
		if dead {
			return errWatchLost
		}
	}
}
