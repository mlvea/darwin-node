package hostport

import (
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
)

func TestHalfCloseLetsGuestFinishResponse(t *testing.T) {
	leakcheck.Check(t)
	guestLn := listenLocal(t)
	guestPort := guestLn.Addr().(*net.TCPAddr).Port
	got := make(chan []byte, 1)
	go func() {
		c, err := guestLn.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf, err := io.ReadAll(c)
		if err != nil {
			return
		}
		if _, err := c.Write([]byte("pong")); err != nil {
			return
		}
		got <- append([]byte(nil), buf...)
	}()

	hostPort := freePort(t)
	m := New(WithIdleLinger(2 * time.Second))
	reserveBind(t, m, "pod", hostPort, guestPort)
	t.Cleanup(func() { m.Release("pod") })

	c, err := net.DialTimeout("tcp", hostAddr(hostPort), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	if err := closeWrite(c); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	reply, err := io.ReadAll(c)
	if err != nil || string(reply) != "pong" {
		t.Fatalf("client reply %q err %v; the guest never saw EOF or the proxy closed it before the response", reply, err)
	}
	select {
	case body := <-got:
		if string(body) != "ping" {
			t.Fatalf("guest read %q", body)
		}
	case <-time.After(time.Second):
		t.Fatal("guest did not finish reading")
	}
}

func TestSilentPeerClosesAfterLinger(t *testing.T) {
	leakcheck.Check(t)
	linger := 300 * time.Millisecond
	guestLn := listenLocal(t)
	guestPort := guestLn.Addr().(*net.TCPAddr).Port
	type guestEOF struct {
		data []byte
		at   time.Time
	}
	eofc := make(chan guestEOF, 1)
	var hold net.Conn
	var holdMu sync.Mutex
	go func() {
		c, err := guestLn.Accept()
		if err != nil {
			return
		}
		holdMu.Lock()
		hold = c
		holdMu.Unlock()
		buf, _ := io.ReadAll(c)
		eofc <- guestEOF{data: append([]byte(nil), buf...), at: time.Now()}
	}()
	t.Cleanup(func() {
		holdMu.Lock()
		c := hold
		holdMu.Unlock()
		if c != nil {
			_ = c.Close()
		}
	})

	hostPort := freePort(t)
	m := New(WithIdleLinger(linger))
	reserveBind(t, m, "pod", hostPort, guestPort)
	t.Cleanup(func() { m.Release("pod") })

	c, err := net.DialTimeout("tcp", hostAddr(hostPort), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	closedAt := time.Now()
	if err := closeWrite(c); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(linger + 4*time.Second))
	_, rerr := c.Read(make([]byte, 8))
	elapsed := time.Since(closedAt)
	if rerr == nil || elapsed > linger+3*time.Second {
		t.Fatalf("client still open after %s (err %v); one side ended and the other was not closed", elapsed, rerr)
	}
	if elapsed < linger/2 {
		t.Fatalf("client closed after %s, before the %s linger", elapsed, linger)
	}
	select {
	case g := <-eofc:
		if string(g.data) != "ping" {
			t.Fatalf("guest read %q", g.data)
		}
		if g.at.After(closedAt.Add(elapsed)) {
			t.Fatalf("guest saw EOF at %s, after the client was fully closed", g.at.Sub(closedAt))
		}
	case <-time.After(linger + 4*time.Second):
		t.Fatal("guest did not observe EOF from the half-close")
	}
}

func TestGuestResetClosesClient(t *testing.T) {
	leakcheck.Check(t)
	guestLn := listenLocal(t)
	guestPort := guestLn.Addr().(*net.TCPAddr).Port
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := guestLn.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()

	hostPort := freePort(t)
	m := New(WithIdleLinger(2 * time.Second))
	reserveBind(t, m, "pod", hostPort, guestPort)
	t.Cleanup(func() { m.Release("pod") })

	c, err := net.DialTimeout("tcp", hostAddr(hostPort), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var guest net.Conn
	select {
	case guest = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("guest did not accept")
	}
	t.Cleanup(func() { _ = guest.Close() })
	tc, ok := guest.(*net.TCPConn)
	if !ok {
		t.Fatalf("guest conn %T", guest)
	}
	if err := tc.SetLinger(0); err != nil {
		t.Fatal(err)
	}
	if err := tc.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, rerr := c.Read(make([]byte, 8))
	if rerr == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("client stayed open for %s after the guest reset (err %v)", time.Since(start), rerr)
	}
}

func TestPipeCloseUnblocksTheOtherSide(t *testing.T) {
	leakcheck.Check(t)
	linger := 200 * time.Millisecond
	client, clientSide := net.Pipe()
	guest, guestSide := net.Pipe()
	done := make(chan struct{})
	go func() {
		join(clientSide, guestSide, linger)
		close(done)
	}()
	t.Cleanup(func() {
		_ = client.Close()
		_ = guest.Close()
		_ = clientSide.Close()
		_ = guestSide.Close()
	})
	if err := guest.Close(); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := client.Read(make([]byte, 1))
	elapsed := time.Since(start)
	if err == nil || elapsed > linger+time.Second {
		t.Fatalf("other side still open after %s (err %v)", elapsed, err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("join did not return")
	}
}

func TestReleaseClosesLiveConnections(t *testing.T) {
	leakcheck.Check(t)
	guestLn := listenLocal(t)
	guestPort := guestLn.Addr().(*net.TCPAddr).Port
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := guestLn.Accept()
		if err != nil {
			return
		}
		accepted <- c
	}()

	hostPort := freePort(t)
	m := New(WithIdleLinger(5 * time.Second))
	reserveBind(t, m, "pod", hostPort, guestPort)

	c, err := net.DialTimeout("tcp", hostAddr(hostPort), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var guest net.Conn
	select {
	case guest = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("guest did not accept")
	}
	defer guest.Close()

	// Both sides are idle. Release has to close them now, not after linger.
	start := time.Now()
	m.Release("pod")
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	_, cerr := c.Read(make([]byte, 8))
	_ = guest.SetReadDeadline(time.Now().Add(time.Second))
	_, gerr := guest.Read(make([]byte, 8))
	elapsed := time.Since(start)
	if cerr == nil || gerr == nil || elapsed > 500*time.Millisecond {
		t.Fatalf("after Release (%s) client err %v guest err %v", elapsed, cerr, gerr)
	}
	if _, err := net.DialTimeout("tcp", hostAddr(hostPort), 200*time.Millisecond); err == nil {
		t.Fatal("listener still accepting after Release")
	}
}

func TestGuestDialTimesOut(t *testing.T) {
	leakcheck.Check(t)
	addr, release := guestDialTarget(t)
	defer release()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	guestPort, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	guestIP, _, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	hostPort := freePort(t)
	timeout := 300 * time.Millisecond
	m := New(WithDialTimeout(timeout))
	reserveBindTo(t, m, "pod", hostPort, guestIP, guestPort)
	t.Cleanup(func() { m.Release("pod") })

	start := time.Now()
	c, err := net.DialTimeout("tcp", hostAddr(hostPort), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, rerr := c.Read(make([]byte, 8))
	elapsed := time.Since(start)
	if rerr == nil || elapsed < timeout/2 || elapsed > timeout+2*time.Second {
		t.Fatalf("host side closed after %s (err %v); dial timeout is %s", elapsed, rerr, timeout)
	}
}

func TestPerPodConnectionCap(t *testing.T) {
	leakcheck.Check(t)
	guestPort, accepted, bag := holdGuest(t)
	otherPort, otherAccepted, _ := holdGuest(t)
	hostPort := freePort(t)
	otherHost := freePort(t)
	m := New(WithMaxConns(2, 10), WithDialTimeout(time.Second), WithIdleLinger(time.Second))
	reserveBind(t, m, "pod", hostPort, guestPort)
	reserveBind(t, m, "other", otherHost, otherPort)
	t.Cleanup(func() {
		m.Release("pod")
		m.Release("other")
	})

	var held []net.Conn
	t.Cleanup(func() {
		for _, c := range held {
			_ = c.Close()
		}
	})
	for i := 0; i < 2; i++ {
		c, err := net.DialTimeout("tcp", hostAddr(hostPort), time.Second)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
	}
	waitAccepts(t, accepted, 2)

	start := time.Now()
	extra, err := net.DialTimeout("tcp", hostAddr(hostPort), time.Second)
	if err == nil {
		defer extra.Close()
		_ = extra.SetReadDeadline(time.Now().Add(time.Second))
		_, err = extra.Read(make([]byte, 1))
	}
	elapsed := time.Since(start)
	if err == nil || elapsed > 500*time.Millisecond {
		t.Fatalf("third connection stayed open for %s (err %v)", elapsed, err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := accepted.Load(); got != 2 {
		t.Fatalf("guest accepted %d connections, want 2", got)
	}

	// Another pod still has room under its own cap.
	oc, err := net.DialTimeout("tcp", hostAddr(otherHost), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer oc.Close()
	waitAccepts(t, otherAccepted, 1)

	// Closing one live splice frees the pod's slot.
	_ = held[0].Close()
	bag.closeOne()
	deadline := time.Now().Add(4 * time.Second)
	for accepted.Load() < 3 && time.Now().Before(deadline) {
		c, derr := net.DialTimeout("tcp", hostAddr(hostPort), time.Second)
		if derr != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		time.Sleep(30 * time.Millisecond)
		if accepted.Load() >= 3 {
			_ = c.Close()
			break
		}
		_ = c.Close()
		time.Sleep(20 * time.Millisecond)
	}
	if got := accepted.Load(); got < 3 {
		t.Fatalf("guest accepted %d after a slot was freed, want at least 3", got)
	}
}

func TestGlobalConnectionCap(t *testing.T) {
	leakcheck.Check(t)
	portA, accA, _ := holdGuest(t)
	portB, accB, _ := holdGuest(t)
	hostA := freePort(t)
	hostB := freePort(t)
	m := New(WithMaxConns(5, 2), WithDialTimeout(time.Second))
	reserveBind(t, m, "a", hostA, portA)
	reserveBind(t, m, "b", hostB, portB)
	t.Cleanup(func() {
		m.Release("a")
		m.Release("b")
	})
	ca, err := net.DialTimeout("tcp", hostAddr(hostA), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer ca.Close()
	cb, err := net.DialTimeout("tcp", hostAddr(hostB), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer cb.Close()
	waitAccepts(t, accA, 1)
	waitAccepts(t, accB, 1)

	start := time.Now()
	extra, err := net.DialTimeout("tcp", hostAddr(hostA), time.Second)
	if err == nil {
		defer extra.Close()
		_ = extra.SetReadDeadline(time.Now().Add(time.Second))
		_, err = extra.Read(make([]byte, 1))
	}
	if err == nil || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("connection over the node cap stayed open for %s (err %v)", time.Since(start), err)
	}
	time.Sleep(150 * time.Millisecond)
	if accA.Load() != 1 || accB.Load() != 1 {
		t.Fatalf("guest accepts a=%d b=%d, want 1 and 1", accA.Load(), accB.Load())
	}
}

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

func hostAddr(port int) string {
	return net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
}

func reserveBind(t *testing.T, m *Manager, pod string, hostPort, guestPort int) {
	t.Helper()
	reserveBindTo(t, m, pod, hostPort, "127.0.0.1", guestPort)
}

func reserveBindTo(t *testing.T, m *Manager, pod string, hostPort int, guestIP string, guestPort int) {
	t.Helper()
	maps := []Mapping{{
		HostIP:        "127.0.0.1",
		HostPort:      hostPort,
		PodIP:         guestIP,
		ContainerPort: guestPort,
		Protocol:      "TCP",
	}}
	if err := m.Reserve(pod, maps); err != nil {
		t.Fatal(err)
	}
	if err := m.Bind(pod, maps); err != nil {
		t.Fatal(err)
	}
}

func closeWrite(c net.Conn) error {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

type connBag struct {
	mu sync.Mutex
	cs []net.Conn
}

func (b *connBag) add(c net.Conn) {
	b.mu.Lock()
	b.cs = append(b.cs, c)
	b.mu.Unlock()
}

func (b *connBag) closeOne() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.cs) == 0 {
		return
	}
	_ = b.cs[0].Close()
	b.cs = b.cs[1:]
}

func (b *connBag) closeAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, c := range b.cs {
		_ = c.Close()
	}
	b.cs = nil
}

func holdGuest(t *testing.T) (int, *atomic.Int32, *connBag) {
	t.Helper()
	ln := listenLocal(t)
	var n atomic.Int32
	bag := &connBag{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			bag.add(c)
		}
	}()
	t.Cleanup(bag.closeAll)
	return ln.Addr().(*net.TCPAddr).Port, &n, bag
}

func waitAccepts(t *testing.T, n *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() < want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := n.Load(); got < want {
		t.Fatalf("guest accepted %d, want %d", got, want)
	}
}

// guestDialTarget is an address that does not complete a dial promptly.
// A zero-backlog loopback listener is preferred. TEST-NET-1 is the fallback
// when that queue cannot be filled, which is a blackhole on a normal host.
func guestDialTarget(t *testing.T) (string, func()) {
	t.Helper()
	if addr, release, ok := fullAcceptQueue(); ok {
		return addr, release
	}
	return "192.0.2.1:81", func() {}
}

// fullAcceptQueue listens with a zero backlog and holds connections until a
// further dial blocks. ok is false when the queue never fills.
func fullAcceptQueue() (addr string, release func(), ok bool) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return "", func() {}, false
	}
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		_ = syscall.Close(fd)
		return "", func() {}, false
	}
	if err := syscall.Bind(fd, &syscall.SockaddrInet4{Port: 0, Addr: [4]byte{127, 0, 0, 1}}); err != nil {
		_ = syscall.Close(fd)
		return "", func() {}, false
	}
	if err := syscall.Listen(fd, 0); err != nil {
		_ = syscall.Close(fd)
		return "", func() {}, false
	}
	sa, err := syscall.Getsockname(fd)
	if err != nil {
		_ = syscall.Close(fd)
		return "", func() {}, false
	}
	ina, okAddr := sa.(*syscall.SockaddrInet4)
	if !okAddr {
		_ = syscall.Close(fd)
		return "", func() {}, false
	}
	addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(ina.Port))
	var held []net.Conn
	filled := false
	for i := 0; i < 64; i++ {
		c, derr := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if derr != nil {
			filled = true
			break
		}
		held = append(held, c)
	}
	release = func() {
		for _, c := range held {
			_ = c.Close()
		}
		_ = syscall.Close(fd)
	}
	if !filled {
		release()
		return "", func() {}, false
	}
	return addr, release, true
}
