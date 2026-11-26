package simvz

import (
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// flowGroup is the host end of live virtio-vsock connections.
//
// While the VM is Running the peer is drained, so host writes complete.
// Pause and Save stop that drain: the fd stays open, writes succeed until
// the socket buffer fills and then block, and reads block with no EOF.
// Stop and crash close the peer, so reads return EOF and writes return
// EPIPE or ECONNRESET. A restored VM does not inherit the old conns.
type flowGroup struct {
	mu      sync.Mutex
	cond    *sync.Cond
	paused  bool
	dead    bool
	closed  bool
	waiting int
	pumps   int
	conns   []net.Conn
	wg      sync.WaitGroup
}

func newFlowGroup() *flowGroup {
	g := &flowGroup{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *flowGroup) add(peer net.Conn) {
	if g == nil || peer == nil {
		return
	}
	g.mu.Lock()
	if g.dead {
		g.mu.Unlock()
		_ = peer.Close()
		return
	}
	g.pumps++
	g.conns = append(g.conns, peer)
	g.mu.Unlock()
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		defer func() {
			g.mu.Lock()
			g.pumps--
			g.cond.Broadcast()
			g.mu.Unlock()
		}()
		g.pump(peer)
	}()
}

func (g *flowGroup) pump(peer net.Conn) {
	buf := make([]byte, 2048)
	g.mu.Lock()
	for {
		if g.dead {
			g.mu.Unlock()
			return
		}
		if g.paused {
			g.waiting++
			g.cond.Broadcast()
			for g.paused && !g.dead {
				g.cond.Wait()
			}
			g.waiting--
			continue
		}
		g.mu.Unlock()
		_ = peer.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
		_, err := peer.Read(buf)
		g.mu.Lock()
		if err == nil {
			continue
		}
		if isTimeout(err) {
			continue
		}
		g.mu.Unlock()
		return
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// setPaused parks every pump before returning when p is true, so a write
// that starts after Pause returns is not drained.
func (g *flowGroup) setPaused(p bool) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dead {
		return
	}
	g.paused = p
	if !p {
		g.cond.Broadcast()
		return
	}
	for _, c := range g.conns {
		_ = c.SetReadDeadline(time.Now())
	}
	for !g.dead && g.waiting < g.pumps {
		g.cond.Wait()
	}
}

func (g *flowGroup) close() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.closed {
		g.mu.Unlock()
		return
	}
	g.closed = true
	g.dead = true
	g.paused = false
	conns := append([]net.Conn(nil), g.conns...)
	for _, c := range conns {
		_ = c.SetReadDeadline(time.Now())
	}
	g.cond.Broadcast()
	g.mu.Unlock()
	g.wg.Wait()
	for _, c := range conns {
		// Unread bytes make a Unix close look like a reset. Drain first so
		// the host read is EOF, which is the contract for Stop and crash.
		drainThenClose(c)
	}
}

func drainThenClose(c net.Conn) {
	if c == nil {
		return
	}
	buf := make([]byte, 8192)
	_ = c.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	for i := 0; i < 256; i++ {
		if _, err := c.Read(buf); err != nil {
			break
		}
	}
	_ = c.Close()
}

func (m *machine) dropFlows() {
	if m == nil {
		return
	}
	m.mu.Lock()
	old := m.flow
	ng := newFlowGroup()
	m.flow = ng
	// Keep the current generation's pointer aligned. A generation that
	// Stop already captured keeps the flow it had; this writes only
	// the boot that is current now.
	if m.gen != nil {
		m.gen.flow = ng
	}
	m.mu.Unlock()
	if old != nil {
		old.close()
	}
}

func (m *machine) currentFlow() *flowGroup {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.flow == nil {
		m.flow = newFlowGroup()
	}
	return m.flow
}

// streamPair is the simulated virtio-vsock fd. The buffer is small so a
// paused peer fills it without a multi-megabyte write. The kernel may
// clamp the request; any finite buffer is enough for the contract.
func streamPair() (host, peer net.Conn, err error) {
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		return nil, nil, err
	}
	unix.CloseOnExec(fds[0])
	unix.CloseOnExec(fds[1])
	for _, fd := range fds {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_SNDBUF, 8*1024)
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, 8*1024)
	}
	hf := os.NewFile(uintptr(fds[0]), "simvz-vsock-host")
	pf := os.NewFile(uintptr(fds[1]), "simvz-vsock-peer")
	host, err = net.FileConn(hf)
	if err != nil {
		_ = hf.Close()
		_ = pf.Close()
		return nil, nil, err
	}
	peer, err = net.FileConn(pf)
	_ = hf.Close()
	_ = pf.Close()
	if err != nil {
		_ = host.Close()
		return nil, nil, err
	}
	return host, peer, nil
}
