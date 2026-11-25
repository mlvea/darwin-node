//go:build linux

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
		// Linux SO_PEERCRED on a socket that was never connected returns
		// success and a uid of ^uint32(0). That is not a peer. ENOTCONN
		// from getpeername is the same case: close quietly.
		if _, e := unix.Getpeername(int(fd)); e != nil {
			gerr = e
			return
		}
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
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
