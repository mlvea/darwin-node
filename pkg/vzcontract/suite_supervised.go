package vzcontract

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/supervised"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"
	"go.uber.org/goleak"
)

// RunSupervisedAdopt locks the node-agent adapter to supervisor.Adopt.
// agent_restart: a new supervised.Runtime adopts the live VM with a second
// Connect and does not boot another VM. supervisor_exit: the slot is
// failed with "supervisor lost" within 2s of the supervisor process
// leaving. SIGKILL of a real child is pkg/runtime/supervised; this
// process has no Virtualization.framework backend to re-exec.
func RunSupervisedAdopt(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("agent_restart", func(t *testing.T) { testSupervisedReadopt(t, factory) })
	t.Run("supervisor_exit", func(t *testing.T) { testSupervisedSupervisorExit(t, factory) })
}

func testSupervisedReadopt(t *testing.T, factory Factory) {
	leaks := goleak.IgnoreCurrent()
	prepare(t)
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })

	dir := shortRunDir(t)
	br := newCountingBridge(t, factory)
	stop := startInProcSupervisor(t, dir, br)
	t.Cleanup(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	first := supervisedRuntime(dir, "agent-old")
	m, err := first.Create(ctx, supervisedSpec())
	if err != nil {
		t.Fatal(err)
	}
	var second runtime.Machine
	t.Cleanup(func() {
		if second != nil {
			_ = second.Stop(context.Background(), 0)
		}
		_ = m.Stop(context.Background(), 0)
	})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if br.starts() != 1 || len(br.dials()) != 1 {
		t.Fatalf("starts %d dials %v: first attach must Adopt one Connect", br.starts(), br.dials())
	}
	if m.Status().State != types.VMRunning {
		t.Fatalf("status %+v", m.Status())
	}

	next := supervisedRuntime(dir, "agent-new")
	got, err := next.Recover(ctx, []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-restart", Found: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("recovered %d", len(got))
	}
	second = got[0]
	dials := br.dials()
	if br.starts() != 1 || len(dials) != 2 || dials[0] == dials[1] {
		t.Fatalf("starts %d dials %v: re-adopt booted a VM or reused a Connect", br.starts(), dials)
	}
	if second.Status().State != types.VMRunning {
		t.Fatalf("readopt status %+v", second.Status())
	}
}

func testSupervisedSupervisorExit(t *testing.T, factory Factory) {
	leaks := goleak.IgnoreCurrent()
	prepare(t)
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })

	dir := shortRunDir(t)
	br := newCountingBridge(t, factory)
	ctx, cancel := context.WithCancel(context.Background())
	srv := &supervisor.Server{
		Slot:          0,
		RunDir:        dir,
		RT:            br,
		BinaryVersion: "sv-exit",
		Deadman:       time.Hour,
	}
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = srv.Serve(ctx)
		close(done)
	}()
	waitListen(t, supervisor.SocketPath(dir, 0), done, func() error { return serveErr })
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("supervisor did not exit")
		}
	})

	actx, acancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer acancel()
	rt := supervisedRuntime(dir, "agent-live")
	m, err := rt.Create(actx, supervisedSpec())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background(), 0) })
	if err := m.Start(actx); err != nil {
		t.Fatal(err)
	}
	if m.Status().State != types.VMRunning {
		t.Fatalf("before exit %+v", m.Status())
	}

	cancel()
	started := time.Now()
	deadline := started.Add(2 * time.Second)
	var st types.VMStatus
	for {
		st = m.Status()
		if st.State == types.VMFailed && st.Message == "supervisor lost" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %+v after %s, want Failed %q within 2s", st, time.Since(started), "supervisor lost")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("supervisor exit took %s", time.Since(started))
	}
}

func supervisedRuntime(dir, version string) *supervised.Runtime {
	return supervised.New(supervised.Options{
		RunDir:        dir,
		Slots:         1,
		Deadman:       time.Hour,
		VMRuntime:     "vzcontract",
		BinaryVersion: version,
	})
}

func supervisedSpec() types.VMSpec {
	return types.VMSpec{
		ID:          types.MachineID{Namespace: "ns", Name: "pod", UID: "uid-restart"},
		ImageRef:    "sha256:restart",
		CPU:         2,
		MemoryBytes: 1 << 30,
		AgentToken:  "tok",
	}
}

// countingBridge is the supervisor backend for the adapter contract.
// Every OpenChannels is one Connect. Start is one VM boot.
type countingBridge struct {
	rt     Runtime
	mu     sync.Mutex
	nStart int
	inos   []uint64
}

func newCountingBridge(t *testing.T, factory Factory) *countingBridge {
	t.Helper()
	if factory == nil {
		t.Fatal("nil factory")
	}
	return &countingBridge{rt: factory(t, Config{})}
}

func (b *countingBridge) starts() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.nStart
}

func (b *countingBridge) dials() []uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]uint64, len(b.inos))
	copy(out, b.inos)
	return out
}

func (b *countingBridge) Name() types.RuntimeName { return "vzcontract" }

func (b *countingBridge) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
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
	return &countingVM{vm: vm, bridge: b, id: spec.ID}, nil
}

type countingVM struct {
	vm     VM
	bridge *countingBridge
	id     types.MachineID
}

func (m *countingVM) ID() types.MachineID { return m.id }

func (m *countingVM) Start(ctx context.Context) error {
	if err := m.vm.Start(ctx); err != nil {
		return err
	}
	m.bridge.mu.Lock()
	m.bridge.nStart++
	m.bridge.mu.Unlock()
	return nil
}

func (m *countingVM) Stop(ctx context.Context, _ time.Duration) error {
	err := m.vm.Stop(ctx)
	if err == nil || m.vm.State() == "Stopped" || m.vm.State() == "Error" {
		return nil
	}
	return err
}

func (m *countingVM) Status() types.VMStatus {
	return types.VMStatus{State: types.VMState(m.vm.State())}
}

func (m *countingVM) DialAgent(context.Context) (*guest.Client, error) {
	return nil, fmt.Errorf("counting bridge dials through OpenChannels")
}

func (m *countingVM) Logs() io.ReadCloser {
	return io.NopCloser(strings.NewReader(""))
}

func (m *countingVM) OpenChannels(ctx context.Context) (*os.File, *os.File, error) {
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
	m.bridge.inos = append(m.bridge.inos, ino)
	m.bridge.mu.Unlock()
	return nic, agent, nil
}

func (m *countingVM) CloseChannels() error { return nil }

var (
	_ runtime.Runtime       = (*countingBridge)(nil)
	_ runtime.Machine       = (*countingVM)(nil)
	_ runtime.ChannelSource = (*countingVM)(nil)
	_ runtime.ChannelCloser = (*countingVM)(nil)
)
