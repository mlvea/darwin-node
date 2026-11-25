package simvz

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// datagramPair is the stand-in for VZFileHandleNetworkDeviceAttachment:
// one connected SOCK_DGRAM socketpair. The host keeps both ends. Close
// them when the VM stops; the kernel does not reap an end this process
// still holds.
func datagramPair() (*os.File, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, datagramType(), 0)
	if err != nil {
		return nil, nil, fmt.Errorf("simvz nic socketpair: %w", err)
	}
	// Linux already passed SOCK_CLOEXEC. Darwin's x/sys has no such flag
	// in this module, so set it here. A second call on Linux is a no-op.
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
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
