//go:build darwin

package supervisor

import "golang.org/x/sys/unix"

// procHandle is a kqueue registered for EVFILT_PROC NOTE_EXIT.
// EV_RECEIPT reports ESRCH when the pid is already gone. Data 0 with
// EV_ERROR means the registration succeeded.
type procHandle struct {
	kq  int
	pid int
}

func openProcWatch(pid int) (*procHandle, error) {
	if pid <= 0 {
		return nil, unix.ESRCH
	}
	kq, err := unix.Kqueue()
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(kq)
	change := unix.Kevent_t{
		Ident:  uint64(pid),
		Filter: unix.EVFILT_PROC,
		Flags:  unix.EV_ADD | unix.EV_RECEIPT,
		Fflags: unix.NOTE_EXIT,
	}
	out := []unix.Kevent_t{{}}
	n, err := unix.Kevent(kq, []unix.Kevent_t{change}, out, nil)
	if err != nil {
		_ = unix.Close(kq)
		return nil, err
	}
	if n > 0 && out[0].Flags&unix.EV_ERROR != 0 && out[0].Data != 0 {
		_ = unix.Close(kq)
		return nil, unix.Errno(out[0].Data)
	}
	return &procHandle{kq: kq, pid: pid}, nil
}

func (h *procHandle) close() error {
	if h == nil || h.kq < 0 {
		return nil
	}
	err := unix.Close(h.kq)
	h.kq = -1
	return err
}

func (h *procHandle) wait(stopFD int) (bool, error) {
	defer h.close()
	if stopFD >= 0 {
		change := unix.Kevent_t{
			Ident:  uint64(stopFD),
			Filter: unix.EVFILT_READ,
			Flags:  unix.EV_ADD | unix.EV_RECEIPT,
		}
		out := []unix.Kevent_t{{}}
		n, err := unix.Kevent(h.kq, []unix.Kevent_t{change}, out, nil)
		if err != nil {
			return false, err
		}
		if n > 0 && out[0].Flags&unix.EV_ERROR != 0 && out[0].Data != 0 {
			return false, unix.Errno(out[0].Data)
		}
	}
	buf := make([]unix.Kevent_t, 2)
	for {
		n, err := unix.Kevent(h.kq, nil, buf, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return false, err
		}
		for i := 0; i < n; i++ {
			ev := buf[i]
			if ev.Filter == unix.EVFILT_PROC && ev.Ident == uint64(h.pid) {
				return true, nil
			}
			if stopFD >= 0 && ev.Filter == unix.EVFILT_READ && int(ev.Ident) == stopFD {
				return false, nil
			}
		}
	}
}
