// Package hostport is a userspace proxy from host Port to guest PodIP:containerPort.
package hostport

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultDialTimeout is how long a new hostPort connection waits for
	// the guest to accept. Past this the host side is closed.
	DefaultDialTimeout = 5 * time.Second
	// DefaultIdleLinger is how long the surviving direction stays open
	// after the other side ends. Then both sides are closed.
	DefaultIdleLinger = 5 * time.Second
	// DefaultMaxConnsPerPod is the number of hostPort connections one pod
	// may hold, including a dial that has not finished. The next one is
	// closed without dialing the guest.
	DefaultMaxConnsPerPod = 256
	// DefaultMaxConns is the node-wide cap across every pod.
	DefaultMaxConns = 512
)

// Option configures a Manager. The zero Option set uses the defaults above.
type Option func(*Manager)

// WithDialTimeout sets the guest dial bound. d <= 0 keeps DefaultDialTimeout.
func WithDialTimeout(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.dialTimeout = d
		}
	}
}

// WithIdleLinger sets how long the surviving direction stays open after the
// other side ends. d <= 0 keeps DefaultIdleLinger.
func WithIdleLinger(d time.Duration) Option {
	return func(m *Manager) {
		if d > 0 {
			m.linger = d
		}
	}
}

// WithMaxConns sets the per-pod and node-wide connection caps. A
// non-positive value keeps that side's default.
func WithMaxConns(perPod, global int) Option {
	return func(m *Manager) {
		if perPod > 0 {
			m.maxPerPod = perPod
		}
		if global > 0 {
			m.maxGlobal = global
		}
	}
}

// Mapping is one kubelet-style hostPort.
type Mapping struct {
	HostIP        string
	HostPort      int
	PodIP         string
	ContainerPort int
	Protocol      string // tcp
}

// proxy is one listening host port.
type proxy struct {
	pod string
	ln  net.Listener
	dst atomic.Value // string host:port
}

// live tracks the connections spliced for one pod. Release closes them.
type live struct {
	ctx    context.Context
	cancel context.CancelFunc
	conns  map[net.Conn]struct{}
	slots  int
	dead   bool
}

// Manager tracks hostPort reservations, listeners, and the connections
// spliced through them.
type Manager struct {
	mu          sync.Mutex
	proxies     map[string]*proxy // key host:port
	reserved    map[string]string // key -> pod
	pods        map[string]*live
	dialTimeout time.Duration
	linger      time.Duration
	maxPerPod   int
	maxGlobal   int
	globalN     int
}

func New(opts ...Option) *Manager {
	m := &Manager{
		proxies:     map[string]*proxy{},
		reserved:    map[string]string{},
		pods:        map[string]*live{},
		dialTimeout: DefaultDialTimeout,
		linger:      DefaultIdleLinger,
		maxPerPod:   DefaultMaxConnsPerPod,
		maxGlobal:   DefaultMaxConns,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(m)
		}
	}
	return m
}

// join copies in both directions. When one direction hits EOF or an error
// it half-closes the other, then fully closes both after linger if the
// surviving direction is still open.
func join(a, b net.Conn, linger time.Duration) {
	if linger <= 0 {
		linger = DefaultIdleLinger
	}
	var wg sync.WaitGroup
	wg.Add(2)
	var once sync.Once
	var timer *time.Timer
	arm := func() {
		once.Do(func() {
			timer = time.AfterFunc(linger, func() {
				_ = a.Close()
				_ = b.Close()
			})
		})
	}
	copyDir := func(dst, src net.Conn) {
		defer wg.Done()
		_, _ = io.Copy(dst, src)
		shutdownWrite(dst)
		_ = dst.SetReadDeadline(time.Now().Add(linger))
		arm()
	}
	go copyDir(b, a)
	go copyDir(a, b)
	wg.Wait()
	if timer != nil {
		timer.Stop()
	}
	_ = a.Close()
	_ = b.Close()
}

func shutdownWrite(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}

func key(host string, port int) string {
	if host == "" {
		host = "0.0.0.0"
	}
	return fmt.Sprintf("%s:%d", host, port)
}

// Reserve fails closed on conflict. Actual listen happens in Bind.
func (m *Manager) Reserve(pod string, maps []Mapping) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mp := range maps {
		if err := checkTCP(mp); err != nil {
			return err
		}
		k := key(mp.HostIP, mp.HostPort)
		if owner, ok := m.reserved[k]; ok && owner != pod {
			return fmt.Errorf("hostPort %s already used by %s", k, owner)
		}
	}
	for _, mp := range maps {
		m.reserved[key(mp.HostIP, mp.HostPort)] = pod
	}
	m.ensurePodLocked(pod)
	return nil
}

// Release drops pod's reservations, closes its listeners, and closes every
// connection still spliced for that pod. Engine.teardown calls this when
// the pod is deleted or has failed, so a removed hostPort does not leave
// the guest dialed.
func (m *Manager) Release(pod string) {
	m.mu.Lock()
	var lns []net.Listener
	var conns []net.Conn
	var cancel context.CancelFunc
	for k, owner := range m.reserved {
		if owner == pod {
			delete(m.reserved, k)
		}
	}
	for k, p := range m.proxies {
		if p != nil && p.pod == pod {
			if p.ln != nil {
				lns = append(lns, p.ln)
			}
			delete(m.proxies, k)
		}
	}
	if st := m.pods[pod]; st != nil {
		st.dead = true
		cancel = st.cancel
		for c := range st.conns {
			conns = append(conns, c)
		}
		delete(m.pods, pod)
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	for _, ln := range lns {
		_ = ln.Close()
	}
}

func (m *Manager) Owners() map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]string{}
	for k, v := range m.reserved {
		out[k] = v
	}
	return out
}

func checkTCP(mp Mapping) error {
	proto := strings.ToUpper(mp.Protocol)
	if proto == "" {
		proto = "TCP"
	}
	if proto != "TCP" {
		return fmt.Errorf("hostPort protocol %s is not supported", mp.Protocol)
	}
	if mp.HostPort < 1 || mp.HostPort > 65535 {
		return fmt.Errorf("hostPort %d is invalid", mp.HostPort)
	}
	if mp.ContainerPort < 1 || mp.ContainerPort > 65535 {
		return fmt.Errorf("containerPort %d is invalid", mp.ContainerPort)
	}
	return nil
}

// Bind listens on each mapping and splices TCP to PodIP:containerPort.
// Reservation must already have succeeded at admission; Bind does not Reserve.
func (m *Manager) Bind(pod string, maps []Mapping) error {
	for _, mp := range maps {
		if err := checkTCP(mp); err != nil {
			return err
		}
		addr := key(mp.HostIP, mp.HostPort)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			m.Release(pod)
			return fmt.Errorf("hostPort listen %s: %w", addr, err)
		}
		p := &proxy{pod: pod, ln: ln}
		p.dst.Store(net.JoinHostPort(mp.PodIP, fmt.Sprintf("%d", mp.ContainerPort)))
		m.mu.Lock()
		m.ensurePodLocked(pod)
		m.proxies[addr] = p
		m.mu.Unlock()
		go m.acceptLoop(p)
	}
	return nil
}

// Dest reports the current splice target for a listener, if one is bound.
func (m *Manager) Dest(host string, port int) (string, bool) {
	if m == nil {
		return "", false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.proxies[key(host, port)]
	if !ok || p == nil {
		return "", false
	}
	s, _ := p.dst.Load().(string)
	return s, s != ""
}

// UpdateDest retargets existing listeners for pod after a VM reboot/IP change.
func (m *Manager) UpdateDest(pod string, maps []Mapping) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mp := range maps {
		k := key(mp.HostIP, mp.HostPort)
		if owner := m.reserved[k]; owner != pod {
			continue
		}
		if p, ok := m.proxies[k]; ok {
			p.dst.Store(net.JoinHostPort(mp.PodIP, fmt.Sprintf("%d", mp.ContainerPort)))
		}
	}
}

func (m *Manager) ensurePodLocked(pod string) {
	st := m.pods[pod]
	if st != nil && !st.dead {
		return
	}
	if st != nil {
		st.dead = true
		st.cancel()
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.pods[pod] = &live{ctx: ctx, cancel: cancel, conns: map[net.Conn]struct{}{}}
}

func (m *Manager) acceptLoop(p *proxy) {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go m.serve(p, c)
	}
}

func (m *Manager) serve(p *proxy, c net.Conn) {
	st, ok := m.acquire(p.pod, c)
	if !ok {
		_ = c.Close()
		return
	}
	defer func() {
		m.releaseConn(st, c, true)
		_ = c.Close()
	}()
	dst, _ := p.dst.Load().(string)
	timeout := m.dialTimeout
	if timeout <= 0 {
		timeout = DefaultDialTimeout
	}
	ctx, cancel := context.WithTimeout(st.ctx, timeout)
	up, err := dialGuest(ctx, dst)
	cancel()
	if err != nil {
		return
	}
	if !m.track(st, up) {
		_ = up.Close()
		return
	}
	defer func() {
		m.releaseConn(st, up, false)
		_ = up.Close()
	}()
	linger := m.linger
	if linger <= 0 {
		linger = DefaultIdleLinger
	}
	join(c, up, linger)
}

func dialGuest(ctx context.Context, addr string) (net.Conn, error) {
	if addr == "" {
		return nil, fmt.Errorf("hostPort dial: empty destination")
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// acquire counts c against the pod and node caps and records it. The
// connection must already be the accepted host side.
func (m *Manager) acquire(pod string, c net.Conn) (*live, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.pods[pod]
	if st == nil || st.dead || c == nil {
		return nil, false
	}
	if st.slots >= m.maxPerPod || m.globalN >= m.maxGlobal {
		return nil, false
	}
	st.slots++
	m.globalN++
	st.conns[c] = struct{}{}
	return st, true
}

func (m *Manager) track(st *live, c net.Conn) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if st == nil || st.dead || c == nil {
		return false
	}
	st.conns[c] = struct{}{}
	return true
}

func (m *Manager) releaseConn(st *live, c net.Conn, slot bool) {
	if st == nil || c == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := st.conns[c]; !ok {
		return
	}
	delete(st.conns, c)
	if !slot {
		return
	}
	if st.slots > 0 {
		st.slots--
	}
	if m.globalN > 0 {
		m.globalN--
	}
}
