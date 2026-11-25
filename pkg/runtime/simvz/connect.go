package simvz

import (
	"context"
	"fmt"
	"net"
	"syscall"

	"github.com/mlvea/darwin-node/pkg/guest"
)

// Connect dials the guest's virtio-vsock port. The completion is async:
// ConnectDelay (logical time) elapses, then the call returns. It does not
// poll for a listener. If the agent has not registered, the result is
// ECONNREFUSED. A listener created before RegisterAgent is not enough;
// DialAgent uses the same ready flag.
func (m *machine) Connect(ctx context.Context) (net.Conn, error) {
	if m == nil {
		return nil, fmt.Errorf("simvz: nil machine")
	}
	m.mustOutside()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	st := m.state
	m.mu.Unlock()
	switch st {
	case vzRunning, vzPaused, vzSaving, vzPausing, vzResuming:
	default:
		err := &StateError{Op: "connect", State: st.String()}
		m.finishConnect(ctx, err)
		return nil, err
	}
	if err := m.clock.Sleep(ctx, m.faults.ConnectDelay); err != nil {
		return nil, err
	}
	m.mu.Lock()
	ready := m.agentReady
	paused := m.state != vzRunning && m.state != vzResuming
	m.mu.Unlock()
	if !ready {
		err := fmt.Errorf("simvz: connect: %w", syscall.ECONNREFUSED)
		m.finishConnect(ctx, err)
		return nil, err
	}
	host, peer, err := streamPair()
	if err != nil {
		m.finishConnect(ctx, err)
		return nil, err
	}
	g := m.currentFlow()
	if paused {
		g.setPaused(true)
	}
	g.add(peer)
	m.finishConnect(ctx, nil)
	return host, nil
}

func (m *machine) finishConnect(ctx context.Context, err error) {
	m.q.Do(func() {
		m.barrier(ctx, BarrierBeforeConnectCompletion)
		m.recordCompletion(err)
	})
}

// RegisterAgent binds the guest listener. Start does this unless
// Faults.HoldAgent is set. Connecting before this returns ECONNREFUSED.
func (m *machine) RegisterAgent(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("simvz: nil machine")
	}
	m.mustOutside()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	ready := m.agentReady
	st := m.state
	m.mu.Unlock()
	if ready {
		return nil
	}
	switch st {
	case vzRunning, vzPaused, vzStarting, vzResuming, vzSaving, vzPausing:
	default:
		return &StateError{Op: "register", State: st.String()}
	}
	if m.rt != nil && m.rt.guestBin != "" {
		return m.spawnGuest(ctx)
	}
	return m.serveInProcess(m.lifeOrBG())
}

func (m *machine) lifeOrBG() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.life != nil {
		return m.life
	}
	return context.Background()
}

// WriteDatagram sends on the host NIC. The first DatagramENOBUFS calls
// return ENOBUFS and do not queue the payload. A full nonblocking socket
// returns ENOBUFS or EAGAIN from the kernel; it does not drop quietly.
func (m *machine) WriteDatagram(p []byte) error {
	if m == nil {
		return fmt.Errorf("simvz: nil machine")
	}
	m.mu.Lock()
	if m.dgramLeft > 0 {
		m.dgramLeft--
		m.mu.Unlock()
		return syscall.ENOBUFS
	}
	f := m.hostNIC
	m.mu.Unlock()
	if f == nil {
		return fmt.Errorf("simvz: nic is not attached")
	}
	_, err := f.Write(p)
	return err
}

// QuotaHeld reports whether this VM currently counts against the cap.
func (m *machine) QuotaHeld() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held
}

// Crash kills the guest and the live vsock fds. The VM stays in its
// current state and keeps its slot. Further connects fail until the
// agent is registered again.
func (m *machine) Crash() error {
	if m == nil {
		return fmt.Errorf("simvz: nil machine")
	}
	m.mustOutside()
	m.mu.Lock()
	st := m.state
	m.mu.Unlock()
	if st != vzRunning && st != vzPaused {
		return &StateError{Op: "crash", State: st.String()}
	}
	m.crashGuest()
	return nil
}

// DialAgent connects to the guest's unix socket and performs the host
// handshake. The VM must be Running. A guest that has not registered
// fails with ECONNREFUSED and does not block. A crashed guest fails the
// dial and leaves the machine Running.
func (m *machine) DialAgent(ctx context.Context) (*guest.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, fmt.Errorf("simvz: nil machine")
	}
	m.mustOutside()
	m.mu.Lock()
	if m.state != vzRunning {
		st := m.state.String()
		m.mu.Unlock()
		return nil, &StateError{Op: "dial", State: st}
	}
	m.mu.Unlock()
	if m.faults.ConnectDelay > 0 {
		if err := m.clock.Sleep(ctx, m.faults.ConnectDelay); err != nil {
			return nil, err
		}
	}
	m.mu.Lock()
	ready := m.agentReady
	path := m.sock
	token := m.token
	life := m.life
	m.mu.Unlock()
	if !ready || path == "" {
		return nil, fmt.Errorf("simvz: vsock dial: %w", syscall.ECONNREFUSED)
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("simvz: vsock dial: %w", err)
	}
	wrapped := m.wrap(life, ctx, conn)
	cli, err := guest.Dial(ctx, wrapped, token, "simvz")
	if err != nil {
		_ = wrapped.Close()
		return nil, err
	}
	return cli, nil
}
