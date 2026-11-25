//go:build darwin

package supervisor

import (
	"errors"
	"net"

	"golang.org/x/sys/unix"
)

func peerUID(conn *net.UnixConn) (int, error) {
	raw, err := conn.SyscallConn()
	if err != nil {
		return 0, err
	}
	var uid int
	var gerr error
	if err := raw.Control(func(fd uintptr) {
		if _, e := unix.Getpeername(int(fd)); e != nil {
			gerr = e
			return
		}
		// LOCAL_PEERCRED is the macOS peer credential. SO_PEERCRED is Linux.
		cred, e := unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if e != nil {
			gerr = e
			return
		}
		uid = int(cred.Uid)
	}); err != nil {
		return 0, err
	}
	if gerr != nil {
		if errors.Is(gerr, unix.ENOTCONN) {
			return 0, errPeerGone
		}
		return 0, gerr
	}
	return uid, nil
}
