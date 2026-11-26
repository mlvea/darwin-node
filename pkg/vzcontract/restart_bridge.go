package vzcontract

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/types"
)

// restartBridge is the supervisor's VM backend for the in-process restart
// case. Every OpenChannels calls VM.Connect, which is the contract's
// ConnectToPort stand-in, and records the inode of the descriptor it
// hands back.
type restartBridge struct {
	rt    Runtime
	mu    sync.Mutex
	dials []uint64
}

func newRestartBridge(t *testing.T, factory Factory) *restartBridge {
	t.Helper()
	if factory == nil {
		t.Fatal("nil factory")
	}
	return &restartBridge{rt: factory(t, Config{})}
}

func (b *restartBridge) Dials() []uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]uint64, len(b.dials))
	copy(out, b.dials)
	return out
}

func (b *restartBridge) Name() types.RuntimeName { return "vzcontract" }

func (b *restartBridge) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	vm, err := b.rt.Create(ctx, Spec{
		Name:          spec.ID.Name,
		Namespace:     spec.ID.Namespace,
		UID:           spec.ID.UID,
		HardwareModel: spec.HardwareModelData,
		MAC:           spec.MAC,
		CPU:           spec.CPU,
		MemoryBytes:   spec.MemoryBytes,
	})
	if err != nil {
		return nil, err
	}
	return &restartVM{vm: vm, bridge: b, id: spec.ID}, nil
}

type restartVM struct {
	vm     VM
	bridge *restartBridge
	id     types.MachineID
}

func (m *restartVM) ID() types.MachineID { return m.id }

func (m *restartVM) Start(ctx context.Context) error { return m.vm.Start(ctx) }

func (m *restartVM) Stop(ctx context.Context, _ time.Duration) error {
	err := m.vm.Stop(ctx)
	if err == nil || m.vm.State() == "Stopped" || m.vm.State() == "Error" {
		return nil
	}
	return err
}

func (m *restartVM) Status() types.VMStatus {
	return types.VMStatus{State: types.VMState(m.vm.State())}
}

func (m *restartVM) DialAgent(context.Context) (*guest.Client, error) {
	return nil, fmt.Errorf("restart bridge dials through OpenChannels")
}

func (m *restartVM) Logs() io.ReadCloser {
	return io.NopCloser(strings.NewReader(""))
}

func (m *restartVM) OpenChannels(ctx context.Context) (*os.File, *os.File, error) {
	nic, err := dupOSFile(m.vm.HostNIC())
	if err != nil {
		return nil, nil, err
	}
	conn, err := m.vm.Connect(ctx)
	if err != nil {
		_ = nic.Close()
		return nil, nil, err
	}
	agent, err := fileFromConn(conn)
	if err != nil {
		_ = nic.Close()
		return nil, nil, err
	}
	ino, err := fileIno(agent)
	if err != nil {
		_ = nic.Close()
		_ = agent.Close()
		return nil, nil, err
	}
	m.bridge.mu.Lock()
	m.bridge.dials = append(m.bridge.dials, ino)
	m.bridge.mu.Unlock()
	return nic, agent, nil
}

func (m *restartVM) CloseChannels() error { return nil }

func fileFromConn(c net.Conn) (*os.File, error) {
	if c == nil {
		return nil, fmt.Errorf("nil guest connection")
	}
	withFile, ok := c.(interface{ File() (*os.File, error) })
	if !ok {
		_ = c.Close()
		return nil, fmt.Errorf("guest connection has no file descriptor")
	}
	f, err := withFile.File()
	_ = c.Close()
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, fmt.Errorf("guest connection has no file descriptor")
	}
	return f, nil
}

var (
	_ runtime.Runtime       = (*restartBridge)(nil)
	_ runtime.Machine       = (*restartVM)(nil)
	_ runtime.ChannelSource = (*restartVM)(nil)
	_ runtime.ChannelCloser = (*restartVM)(nil)
)
