//go:build linux

package supervisor

import (
	"errors"

	"golang.org/x/sys/unix"
)

// procHandle is a pidfd. It becomes readable when the process exits,
// including when the process is still a zombie.
type procHandle struct {
	fd int
}

func openProcWatch(pid int) (*procHandle, error) {
	if pid <= 0 {
		return nil, unix.ESRCH
	}
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	return &procHandle{fd: fd}, nil
}

func (h *procHandle) close() error {
	if h == nil || h.fd < 0 {
		return nil
	}
	err := unix.Close(h.fd)
	h.fd = -1
	return err
}

func (h *procHandle) wait(stopFD int) (bool, error) {
	defer h.close()
	poll := []unix.PollFd{{
		Fd:     int32(h.fd),
		Events: unix.POLLIN,
	}}
	if stopFD >= 0 {
		poll = append(poll, unix.PollFd{
			Fd:     int32(stopFD),
			Events: unix.POLLIN,
		})
	}
	for {
		_, err := unix.Poll(poll, -1)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return false, err
		}
		if poll[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			return true, nil
		}
		if len(poll) > 1 && poll[1].Revents != 0 {
			return false, nil
		}
	}
}
