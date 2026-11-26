//go:build linux

package supervisor

import "golang.org/x/sys/unix"

// watchFDs blocks until wakeR is readable or a watched connection dies.
// The stream sockets are edge-triggered. A level-triggered peek would
// run again for as long as the guest byte stayed queued, which is the
// whole time the agent has not read it.
func watchFDs(wakeR, ctrlFD, guestFD int) error {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return err
	}
	defer unix.Close(epfd)
	if err := addWatch(epfd, wakeR, unix.EPOLLIN); err != nil {
		return err
	}
	stream := uint32(unix.EPOLLIN | unix.EPOLLRDHUP | unix.EPOLLET)
	if err := addWatch(epfd, ctrlFD, stream); err != nil {
		return err
	}
	if err := addWatch(epfd, guestFD, stream); err != nil {
		return err
	}
	var events [3]unix.EpollEvent
	for {
		n, err := unix.EpollWait(epfd, events[:], -1)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		wake := false
		dead := false
		for i := 0; i < n; i++ {
			fd := int(events[i].Fd)
			if fd == wakeR {
				wake = true
				continue
			}
			if fdDied(fd, events[i].Events) {
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

func addWatch(epfd, fd int, events uint32) error {
	return unix.EpollCtl(epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{
		Events: events,
		Fd:     int32(fd),
	})
}

func fdDied(fd int, ev uint32) bool {
	if ev&(unix.EPOLLERR|unix.EPOLLHUP|unix.EPOLLRDHUP) != 0 {
		return true
	}
	if ev&unix.EPOLLIN == 0 {
		return false
	}
	return deadFD(fd, unix.POLLIN)
}
