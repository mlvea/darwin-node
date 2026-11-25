package simvz

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// NIC buffer goals match pkg/runtime/vz file-handle attachment
// (SO_SNDBUF 256KiB, SO_RCVBUF 1MiB). macOS unix datagram sockets return
// ENOBUFS or EAGAIN when the send buffer is full; they do not drop.
// The kernel may clamp a raise to its ceiling. Callers compare the
// attached socket with an unconfigured one.
const (
	NICSndBuf = 256 * 1024
	NICRcvBuf = 1024 * 1024
)

// datagramPair is the stand-in for VZFileHandleNetworkDeviceAttachment:
// one connected SOCK_DGRAM socketpair. The host keeps both ends. Close
// them when the VM stops; the kernel does not reap an end this process
// still holds. Buffers are raised before either end is returned.
func datagramPair() (*os.File, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, datagramType(), 0)
	if err != nil {
		return nil, nil, fmt.Errorf("simvz nic socketpair: %w", err)
	}
	// Linux already passed SOCK_CLOEXEC. Darwin's x/sys has no such flag
	// in this module, so set it here. A second call on Linux is a no-op.
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	for _, fd := range fds {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, NICSndBuf); err != nil {
			_ = unix.Close(fds[0])
			_ = unix.Close(fds[1])
			return nil, nil, fmt.Errorf("simvz nic SO_SNDBUF: %w", err)
		}
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, NICRcvBuf); err != nil {
			_ = unix.Close(fds[0])
			_ = unix.Close(fds[1])
			return nil, nil, fmt.Errorf("simvz nic SO_RCVBUF: %w", err)
		}
	}
	host := os.NewFile(uintptr(fds[0]), "simvz-nic-host")
	guest := os.NewFile(uintptr(fds[1]), "simvz-nic-guest")
	if host == nil || guest == nil {
		if host != nil {
			_ = host.Close()
		} else {
			_ = unix.Close(fds[0])
		}
		if guest != nil {
			_ = guest.Close()
		} else {
			_ = unix.Close(fds[1])
		}
		return nil, nil, fmt.Errorf("simvz nic socketpair: bad fd")
	}
	return host, guest, nil
}
