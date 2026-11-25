//go:build darwin && arm64

package vz

import (
	"context"
	"fmt"
	"net"
	"os"
	"reflect"
	"sync"
	"unsafe"

	"github.com/Code-Hex/vz/v3"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/types"
	"golang.org/x/sys/unix"
)

// nicFiles is the datagram socketpair for VZFileHandleNetworkDeviceAttachment.
// vm is the end the virtual machine reads. host is the end handed to the
// node agent. Both stay open until the machine stops: the framework may
// not dup the descriptor it is given.
type nicFiles struct {
	once sync.Once
	vm   *os.File
	host *os.File
}

func (n *nicFiles) Close() {
	if n == nil {
		return
	}
	n.once.Do(func() {
		if n.vm != nil {
			_ = n.vm.Close()
		}
		if n.host != nil {
			_ = n.host.Close()
		}
	})
}

func newFileHandleAttachment() (*nicFiles, vz.NetworkDeviceAttachment, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		return nil, nil, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	// The host end is what the agent reads. SO_RCVBUF must be at least
	// twice SO_SNDBUF or the framework drops frames. 1MiB is 4x 256KiB.
	const snd = 256 * 1024
	const rcv = 1024 * 1024
	for _, fd := range fds {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, snd); err != nil {
			_ = unix.Close(fds[0])
			_ = unix.Close(fds[1])
			return nil, nil, err
		}
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, rcv); err != nil {
			_ = unix.Close(fds[0])
			_ = unix.Close(fds[1])
			return nil, nil, err
		}
	}
	// The agent dup is polled. The VM end stays blocking; Virtualization
	// calls Fd, which would detach a poller.
	if err := unix.SetNonblock(fds[1], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		return nil, nil, err
	}
	vmFile := os.NewFile(uintptr(fds[0]), "vz-nic-vm")
	hostFile := os.NewFile(uintptr(fds[1]), "vz-nic-host")
	if vmFile == nil || hostFile == nil {
		if vmFile != nil {
			_ = vmFile.Close()
		}
		if hostFile != nil {
			_ = hostFile.Close()
		}
		return nil, nil, fmt.Errorf("file-handle socket")
	}
	att, err := vz.NewFileHandleNetworkDeviceAttachment(vmFile)
	if err != nil {
		_ = vmFile.Close()
		_ = hostFile.Close()
		return nil, nil, err
	}
	return &nicFiles{vm: vmFile, host: hostFile}, att, nil
}

// OpenChannels dups the host NIC and dials the guest agent port. The
// guest-agent bytes are not read here.
func (m *machine) OpenChannels(ctx context.Context) (*os.File, *os.File, error) {
	if m.nic == nil || m.nic.host == nil {
		return nil, nil, fmt.Errorf("file-handle network is not attached")
	}
	netFD, err := dupOSFile(m.nic.host)
	if err != nil {
		return nil, nil, err
	}
	agentFD, err := m.dialGuestAgent(ctx)
	if err != nil {
		_ = netFD.Close()
		return nil, nil, err
	}
	return netFD, agentFD, nil
}

// CloseChannels releases nothing. OpenChannels returns caller-owned dups,
// and the NIC pair stays open until Stop. A vsock dial still in Connect
// ends when the context passed to OpenChannels is cancelled.
func (m *machine) CloseChannels() error { return nil }

// PauseAndSave pauses the VM and writes its state. SaveMachineStateToPath
// is a macOS 14 API. On macOS 13 the framework returns an error and the
// floor of this process stays macOS 13.
func (m *machine) PauseAndSave(_ context.Context, path string) error {
	if m.vm == nil {
		return fmt.Errorf("vm is not running")
	}
	if err := m.vm.Pause(); err != nil {
		return err
	}
	return m.vm.SaveMachineStateToPath(path)
}

func (m *machine) dialGuestAgent(ctx context.Context) (*os.File, error) {
	devs := m.vm.SocketDevices()
	if len(devs) == 0 {
		return nil, fmt.Errorf("vm has no virtio socket")
	}
	type result struct {
		conn *vz.VirtioSocketConnection
		err  error
	}
	ch := make(chan result, 1)
	// Connect submits on the virtualization queue and does not take a
	// context. If the guest is not listening, v3 may never complete the
	// call. The goroutine below closes a late connection after cancel.
	// That wait is hardware-pending: a guest that never listens leaks
	// the goroutine until the process exits.
	go func() {
		c, err := devs[0].Connect(uint32(types.GuestVsockPort))
		ch <- result{c, err}
	}()
	select {
	case <-ctx.Done():
		go func() {
			r := <-ch
			if r.conn != nil {
				_ = r.conn.Close()
			}
		}()
		return nil, ctx.Err()
	case r := <-ch:
		if r.err != nil {
			if r.conn != nil {
				_ = r.conn.Close()
			}
			return nil, r.err
		}
		return fileFromVirtio(r.conn)
	}
}

// fileFromVirtio dups the unix socket hidden in VirtioSocketConnection.
// v3 does not export it. rawConn is a net.Conn produced by net.FileConn,
// so File dups the descriptor. The virtio conn is closed so only the dup
// remains.
func fileFromVirtio(conn *vz.VirtioSocketConnection) (*os.File, error) {
	if conn == nil {
		return nil, fmt.Errorf("nil vsock connection")
	}
	field := reflect.ValueOf(conn).Elem().FieldByName("rawConn")
	if !field.IsValid() || !field.CanAddr() {
		return nil, fmt.Errorf("virtio socket connection has no rawConn")
	}
	exposed := reflect.NewAt(field.Type(), unsafe.Pointer(field.UnsafeAddr())).Elem()
	nc, _ := exposed.Interface().(net.Conn)
	if nc == nil {
		return nil, fmt.Errorf("virtio socket connection has no file")
	}
	withFile, ok := nc.(interface{ File() (*os.File, error) })
	if !ok {
		return nil, fmt.Errorf("virtio socket connection cannot expose a file")
	}
	f, err := withFile.File()
	if err != nil {
		return nil, err
	}
	_ = conn.Close()
	return f, nil
}

func dupOSFile(f *os.File) (*os.File, error) {
	sc, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var nfd int
	var derr error
	if err := sc.Control(func(fd uintptr) {
		nfd, derr = unix.Dup(int(fd))
	}); err != nil {
		return nil, err
	}
	if derr != nil {
		return nil, derr
	}
	unix.CloseOnExec(nfd)
	if err := unix.SetNonblock(nfd, true); err != nil {
		_ = unix.Close(nfd)
		return nil, err
	}
	out := os.NewFile(uintptr(nfd), f.Name())
	if out == nil {
		_ = unix.Close(nfd)
		return nil, fmt.Errorf("dup file")
	}
	return out, nil
}

var (
	_ runtime.PauseSaver    = (*machine)(nil)
	_ runtime.ChannelSource = (*machine)(nil)
	_ runtime.ChannelCloser = (*machine)(nil)
)
