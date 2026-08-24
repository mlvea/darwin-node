//go:build darwin

package guest

import (
	"fmt"
	"net"
	"os"

	"github.com/darwin-node/darwin-node/pkg/types"
	"golang.org/x/sys/unix"
)

// DialHostVsock connects from the guest to the host vsock listener (CID 2).
func DialHostVsock(port uint32) (net.Conn, error) {
	if port == 0 {
		port = uint32(types.GuestVsockPort)
	}
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_HOST, Port: port}
	if err := unix.Connect(fd, sa); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("vsock connect cid=2 port=%d: %w", port, err)
	}
	f := os.NewFile(uintptr(fd), "vsock")
	conn, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		return nil, err
	}
	return conn, nil
}
