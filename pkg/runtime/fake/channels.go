package fake

import (
	"context"
	"fmt"
	"os"
	"sync"

	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/types"
	"golang.org/x/sys/unix"
)

// Channel files live beside the machine so the fake runtime facade in
// fake.go does not grow a second concern. The supervisor server reaches
// them through runtime.ChannelSource and runtime.PauseSaver.

type chanState struct {
	mu        sync.Mutex
	netSup    *os.File
	netPeer   *os.File
	agentSup  *os.File
	agentPeer *os.File
	cancel    context.CancelFunc
	closed    bool
	bridged   bool
	wg        sync.WaitGroup
	savePath  string
}

var (
	machineChans sync.Map // *machine -> *chanState
	bridgeGuest  sync.Map // *Runtime -> bool
)

func stateFor(m *machine) *chanState {
	v, _ := machineChans.LoadOrStore(m, &chanState{})
	return v.(*chanState)
}

// SetBridgeGuest makes OpenChannels serve the in-process guest handler on
// the agent socket. Protocol tests leave it off so both descriptors stay
// raw and a write on one end can be read on the other. The supervisor
// process turns it on so DialAgent completes the guest handshake.
func (r *Runtime) SetBridgeGuest(on bool) {
	if r == nil {
		return
	}
	if on {
		bridgeGuest.Store(r, true)
		return
	}
	bridgeGuest.Delete(r)
}

func bridgeOn(r *Runtime) bool {
	if r == nil {
		return false
	}
	v, ok := bridgeGuest.Load(r)
	return ok && v.(bool)
}

// OpenChannels returns dups of the supervisor ends. The peer ends stay
// here until CloseChannels.
func (m *machine) OpenChannels(ctx context.Context) (*os.File, *os.File, error) {
	_ = ctx
	m.mu.Lock()
	h := m.handler
	sctx := m.sctx
	state := m.state
	rt := m.rt
	m.mu.Unlock()

	st := stateFor(m)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.closed {
		return nil, nil, fmt.Errorf("channels are closed")
	}
	if st.netSup == nil {
		netSup, netPeer, err := socketpair(unix.SOCK_DGRAM)
		if err != nil {
			return nil, nil, err
		}
		agentSup, agentPeer, err := socketpair(unix.SOCK_STREAM)
		if err != nil {
			_ = netSup.Close()
			_ = netPeer.Close()
			return nil, nil, err
		}
		st.netSup, st.netPeer = netSup, netPeer
		st.agentSup, st.agentPeer = agentSup, agentPeer
	}
	if bridgeOn(rt) && !st.bridged && state == types.VMRunning {
		parent := context.Context(context.Background())
		if sctx != nil {
			parent = sctx
		}
		bctx, cancel := context.WithCancel(parent)
		st.cancel = cancel
		st.bridged = true
		peer := st.agentPeer
		st.wg.Add(1)
		go func() {
			defer st.wg.Done()
			_ = guest.Serve(bctx, peer, h)
		}()
	}
	netFD, err := dupOwned(st.netSup)
	if err != nil {
		return nil, nil, err
	}
	agentFD, err := dupOwned(st.agentSup)
	if err != nil {
		_ = netFD.Close()
		return nil, nil, err
	}
	return netFD, agentFD, nil
}

// CloseChannels closes the socketpairs and waits for a guest bridge to
// leave. Safe to call more than once, and safe when channels were never
// opened.
func (m *machine) CloseChannels() error {
	v, ok := machineChans.Load(m)
	if !ok {
		return nil
	}
	st := v.(*chanState)
	st.mu.Lock()
	st.closed = true
	cancel := st.cancel
	files := []*os.File{st.netSup, st.netPeer, st.agentSup, st.agentPeer}
	st.netSup, st.netPeer = nil, nil
	st.agentSup, st.agentPeer = nil, nil
	st.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
	st.wg.Wait()
	machineChans.Delete(m)
	return nil
}

// PauseAndSave records a pause and writes a marker file. The fake VM has
// no Virtualization.framework save image; the marker is what tests read.
func (m *machine) PauseAndSave(_ context.Context, path string) error {
	if path == "" {
		return fmt.Errorf("empty save path")
	}
	st := m.Status()
	if st.State != types.VMRunning && st.State != types.VMStarting {
		return fmt.Errorf("vm is %s", st.State)
	}
	if err := os.WriteFile(path, []byte("darwin-node pause\n"), 0o600); err != nil {
		return err
	}
	cs := stateFor(m)
	cs.mu.Lock()
	cs.savePath = path
	cs.mu.Unlock()
	return nil
}

// ChannelPeers returns dups of the VM-side ends for uid. The test writes
// the descriptor it received from the supervisor and reads the dup.
func (r *Runtime) ChannelPeers(uid string) (netPeer, agentPeer *os.File, err error) {
	m := r.machineByUID(uid)
	if m == nil {
		return nil, nil, fmt.Errorf("no fake machine %s", uid)
	}
	v, ok := machineChans.Load(m)
	if !ok {
		return nil, nil, fmt.Errorf("channels are not open")
	}
	st := v.(*chanState)
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.netPeer == nil || st.agentPeer == nil {
		return nil, nil, fmt.Errorf("channels are not open")
	}
	netPeer, err = dupOwned(st.netPeer)
	if err != nil {
		return nil, nil, err
	}
	agentPeer, err = dupOwned(st.agentPeer)
	if err != nil {
		_ = netPeer.Close()
		return nil, nil, err
	}
	return netPeer, agentPeer, nil
}

func socketpair(kind int) (*os.File, *os.File, error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, kind, 0)
	if err != nil {
		return nil, nil, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	// os.NewFile polls the descriptor only when it is already non-blocking.
	if err := unix.SetNonblock(fds[0], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		return nil, nil, err
	}
	if err := unix.SetNonblock(fds[1], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		return nil, nil, err
	}
	a := os.NewFile(uintptr(fds[0]), "pair-a")
	b := os.NewFile(uintptr(fds[1]), "pair-b")
	if a == nil || b == nil {
		if a != nil {
			_ = a.Close()
		}
		if b != nil {
			_ = b.Close()
		}
		return nil, nil, fmt.Errorf("socketpair file")
	}
	return a, b, nil
}

func dupOwned(f *os.File) (*os.File, error) {
	fd, err := unix.Dup(int(f.Fd()))
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(fd)
	out := os.NewFile(uintptr(fd), f.Name())
	if out == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("dup file")
	}
	return out, nil
}

var (
	_ runtime.PauseSaver    = (*machine)(nil)
	_ runtime.ChannelSource = (*machine)(nil)
	_ runtime.ChannelCloser = (*machine)(nil)
)

// SavePath is the last PauseAndSave path for uid, when one was written.
func (r *Runtime) SavePath(uid string) string {
	m := r.machineByUID(uid)
	if m == nil {
		return ""
	}
	v, ok := machineChans.Load(m)
	if !ok {
		return ""
	}
	st := v.(*chanState)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.savePath
}
