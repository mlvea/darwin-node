package simvz

import (
	"context"
	"fmt"
	"net"
	"os"

	"github.com/mlvea/darwin-node/pkg/runtime"
	"golang.org/x/sys/unix"
)

// OpenChannels dups the host NIC and dials a new guest connection.
// Each call is a Connect, the same path the contract uses for
// ConnectToPort. The previous dial is not reused.
func (m *machine) OpenChannels(ctx context.Context) (*os.File, *os.File, error) {
	if m == nil {
		return nil, nil, fmt.Errorf("simvz: nil machine")
	}
	host, _ := m.NetworkFiles()
	if host == nil {
		return nil, nil, fmt.Errorf("simvz: nic is not attached")
	}
	netFD, err := dupFD(host)
	if err != nil {
		return nil, nil, err
	}
	conn, err := m.Connect(ctx)
	if err != nil {
		_ = netFD.Close()
		return nil, nil, err
	}
	agent, err := fileFromConn(conn)
	if err != nil {
		_ = netFD.Close()
		return nil, nil, err
	}
	return netFD, agent, nil
}

// CloseChannels releases nothing. OpenChannels returns caller-owned
// files. The NIC and the vsock peers stay open until Stop.
func (m *machine) CloseChannels() error { return nil }

func fileFromConn(c net.Conn) (*os.File, error) {
	if c == nil {
		return nil, fmt.Errorf("simvz: nil guest connection")
	}
	withFile, ok := c.(interface{ File() (*os.File, error) })
	if !ok {
		_ = c.Close()
		return nil, fmt.Errorf("simvz: guest connection has no file descriptor")
	}
	f, err := withFile.File()
	_ = c.Close()
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, fmt.Errorf("simvz: guest connection has no file descriptor")
	}
	return f, nil
}

func dupFD(f *os.File) (*os.File, error) {
	if f == nil {
		return nil, fmt.Errorf("simvz: nil file")
	}
	raw, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var nfd int
	var derr error
	if err := raw.Control(func(fd uintptr) {
		nfd, derr = unix.Dup(int(fd))
	}); err != nil {
		return nil, err
	}
	if derr != nil {
		return nil, derr
	}
	unix.CloseOnExec(nfd)
	out := os.NewFile(uintptr(nfd), f.Name())
	if out == nil {
		_ = unix.Close(nfd)
		return nil, fmt.Errorf("simvz: dup file")
	}
	return out, nil
}

var (
	_ runtime.ChannelSource = (*machine)(nil)
	_ runtime.ChannelCloser = (*machine)(nil)
)
