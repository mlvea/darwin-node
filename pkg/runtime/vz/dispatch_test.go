package vz

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
)

// guestQueue mimics Code-Hex/vz VirtioSocketListener.Listen: one buffered
// accept slot, and every guest connection starts a goroutine that blocks
// in the send until Accept takes it. The goroutine holds the conn, so a
// guest that dials in a loop leaks a goroutine and an fd per extra dial
// once the slot is full.
type guestQueue struct {
	ch     chan net.Conn
	closed chan struct{}
	once   sync.Once
	live   atomic.Int32
}

func newGuestQueue() *guestQueue {
	return &guestQueue{
		ch:     make(chan net.Conn, 1),
		closed: make(chan struct{}),
	}
}

func (q *guestQueue) Accept() (net.Conn, error) {
	select {
	case c := <-q.ch:
		return c, nil
	case <-q.closed:
		return nil, net.ErrClosed
	}
}

func (q *guestQueue) Close() error {
	q.once.Do(func() { close(q.closed) })
	return nil
}

func (q *guestQueue) Addr() net.Addr { return guestQueueAddr{} }

type guestQueueAddr struct{}

func (guestQueueAddr) Network() string { return "vsock" }
func (guestQueueAddr) String() string  { return "2:50051" }

// connect is one guest dial. The returned conn is the host side of the
// pipe, the same object the accept loop receives.
func (q *guestQueue) connect() (*flagConn, net.Conn) {
	client, server := net.Pipe()
	fc := &flagConn{Conn: server}
	q.live.Add(1)
	go func() {
		defer q.live.Add(-1)
		// Close wins over a free buffer slot. Otherwise a dial that
		// arrives after Close can sit in the slot forever.
		select {
		case <-q.closed:
			_ = fc.Close()
			_ = client.Close()
			return
		default:
		}
		select {
		case q.ch <- fc:
		case <-q.closed:
			_ = fc.Close()
			_ = client.Close()
		}
	}()
	return fc, client
}

type flagConn struct {
	net.Conn
	closed atomic.Bool
}

func (c *flagConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

func TestGuestConnsWithoutADialAreClosed(t *testing.T) {
	leakcheck.Check(t)
	q := newGuestQueue()
	d := startAgentDispatch(q)
	t.Cleanup(func() { _ = d.Close() })

	var hosts []*flagConn
	var clients []net.Conn
	for i := 0; i < 8; i++ {
		h, c := q.connect()
		hosts = append(hosts, h)
		clients = append(clients, c)
	}
	t.Cleanup(func() {
		for _, c := range clients {
			_ = c.Close()
		}
	})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		open := 0
		for _, h := range hosts {
			if !h.closed.Load() {
				open++
			}
		}
		if open == 0 && q.live.Load() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	open := 0
	for _, h := range hosts {
		if !h.closed.Load() {
			open++
		}
	}
	t.Fatalf("%d guest conns still open and %d accept goroutines still blocked with no dial waiting", open, q.live.Load())
}

func TestWaitingDialTakesOneConnAndClosesTheRest(t *testing.T) {
	leakcheck.Check(t)
	q := newGuestQueue()
	d := startAgentDispatch(q)
	t.Cleanup(func() { _ = d.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	type got struct {
		c   net.Conn
		err error
	}
	ch := make(chan got, 1)
	go func() {
		c, err := d.Wait(ctx)
		ch <- got{c, err}
	}()
	// Wait has to be inside Accept before the guest dials, or the first
	// conn is "nobody waiting" and the fixed dispatcher closes it.
	time.Sleep(20 * time.Millisecond)
	host, client := q.connect()
	var delivered net.Conn
	select {
	case g := <-ch:
		if g.err != nil {
			t.Fatal(g.err)
		}
		delivered = g.c
	case <-time.After(time.Second):
		t.Fatal("dial did not receive a guest connection")
	}
	// net.Pipe has no buffer: the write has to run beside the read.
	errc := make(chan error, 1)
	go func() {
		_, err := client.Write([]byte("ok"))
		errc <- err
	}()
	buf := make([]byte, 2)
	_ = delivered.SetReadDeadline(time.Now().Add(time.Second))
	n, err := delivered.Read(buf)
	if err != nil || string(buf[:n]) != "ok" {
		t.Fatalf("dial read %q %v", buf[:n], err)
	}
	if werr := <-errc; werr != nil {
		t.Fatal(werr)
	}
	if host.closed.Load() {
		t.Fatal("the connection handed to the dial was closed")
	}
	_ = delivered.Close()
	_ = client.Close()

	var extraH []*flagConn
	var extraC []net.Conn
	for i := 0; i < 5; i++ {
		h, c := q.connect()
		extraH = append(extraH, h)
		extraC = append(extraC, c)
	}
	t.Cleanup(func() {
		for _, c := range extraC {
			_ = c.Close()
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		open := 0
		for _, h := range extraH {
			if !h.closed.Load() {
				open++
			}
		}
		if open == 0 && q.live.Load() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("extra guest conns stayed open after the dial was satisfied (blocked=%d)", q.live.Load())
}

func TestDispatchCloseUnblocksWait(t *testing.T) {
	leakcheck.Check(t)
	q := newGuestQueue()
	d := startAgentDispatch(q)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, err := d.Wait(ctx)
		errc <- err
	}()
	time.Sleep(20 * time.Millisecond)
	if err := d.Close(); err != nil && err != net.ErrClosed {
		t.Fatal(err)
	}
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("Wait returned a conn after Close")
		}
	case <-time.After(time.Second):
		t.Fatal("Wait stayed blocked after Close")
	}
	// A dial that arrives after Close must not stick.
	host, client := q.connect()
	defer client.Close()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if host.closed.Load() && q.live.Load() == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !host.closed.Load() || q.live.Load() != 0 {
		t.Fatalf("late conn closed=%v blocked=%d", host.closed.Load(), q.live.Load())
	}
}
