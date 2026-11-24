package engine

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// reportRuntime serves NetInfo from ip instead of the address the fake
// runtime recorded. hide drops Status IP and MAC so the host has nothing
// assigned to compare against the guest.
type reportRuntime struct {
	*fake.Runtime
	ip   atomic.Value // string
	hide bool
}

func (r *reportRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &reportMachine{Machine: m, token: spec.AgentToken, ip: &r.ip, hide: r.hide}, nil
}

type reportMachine struct {
	runtime.Machine
	token string
	ip    *atomic.Value
	hide  bool
}

func (m *reportMachine) Status() types.VMStatus {
	st := m.Machine.Status()
	if m.hide {
		st.IP = ""
		st.IPs = nil
		st.MAC = ""
	}
	return st
}

func (m *reportMachine) DialAgent(ctx context.Context) (*guest.Client, error) {
	a, b := net.Pipe()
	h := guest.Handler{
		Token:        m.token,
		AgentVersion: "report",
		NetInfoFn: func() (guest.NetInfoRes, error) {
			ip, _ := m.ip.Load().(string)
			return guest.NetInfoRes{PrimaryIP: ip, IPs: []string{ip}}, nil
		},
	}
	h.Init()
	go func() { _ = guest.Serve(context.Background(), a, h) }()
	return guest.Dial(ctx, b, m.token, "test-host")
}

func TestHostPortDoesNotForwardGuestLoopback(t *testing.T) {
	leakcheck.Check(t)
	trap := listenEcho(t)
	hostPort := freePort(t)

	rt := &reportRuntime{Runtime: fake.New(), hide: true}
	rt.ip.Store("127.0.0.1")
	e := newHostPortEngine(t, rt)

	pod := samplePod("hp", "uid-hp")
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{
		ContainerPort: int32(trap.port),
		HostPort:      int32(hostPort),
		Protocol:      corev1.ProtocolTCP,
	}}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Delete(context.Background(), "default", "hp", 0) })
	waitPhase(t, e, "default", "hp", corev1.PodRunning)

	if echo, err := readEcho(net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort))); err == nil && echo == "xy" {
		t.Fatalf("hostPort echoed via 127.0.0.1:%d; the guest named that address and the host had not assigned it (trap hits %d)", trap.port, trap.hits.Load())
	}
	if trap.hits.Load() != 0 {
		t.Fatalf("host loopback accepted %d connection(s) from the hostPort proxy", trap.hits.Load())
	}
}

func TestRestartHostPortDoesNotFollowGuestLoopback(t *testing.T) {
	leakcheck.Check(t)
	trap := listenEcho(t)
	hostPort := freePort(t)

	inner := fake.New()
	inner.IP = "192.168.64.2"
	rt := &reportRuntime{Runtime: inner}
	rt.ip.Store("192.168.64.2")
	e := newHostPortEngine(t, rt)

	pod := samplePod("re", "uid-re")
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{
		ContainerPort: int32(trap.port),
		HostPort:      int32(hostPort),
		Protocol:      corev1.ProtocolTCP,
	}}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Delete(context.Background(), "default", "re", 0) })
	waitPhase(t, e, "default", "re", corev1.PodRunning)

	// The guest now claims the host's loopback. A liveness restart must
	// not retarget the proxy there.
	rt.ip.Store("127.0.0.1")
	e.mu.RLock()
	rec := e.pods["default/re"]
	e.mu.RUnlock()
	if rec == nil {
		t.Fatal("missing record")
	}
	if err := e.restartVM(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	rec.mu.Lock()
	stored := ""
	if rec.pod != nil {
		stored = rec.pod.Status.PodIP
	}
	rec.mu.Unlock()
	if stored == "127.0.0.1" || strings.HasPrefix(stored, "127.") {
		t.Fatalf("stored PodIP %q followed the guest onto loopback", stored)
	}
	dest, ok := e.hostports.Dest("0.0.0.0", hostPort)
	if !ok {
		dest, ok = e.hostports.Dest("", hostPort)
	}
	if !ok {
		t.Fatal("hostPort listener missing after restart")
	}
	want := net.JoinHostPort("192.168.64.2", strconv.Itoa(trap.port))
	if dest != want {
		t.Fatalf("proxy dest %s, want %s", dest, want)
	}
	// Dial only when the proxy claims loopback. A non-local dest would
	// block inside the splice dial, and the assertion above already
	// names the address that would be dialed.
	if strings.HasPrefix(dest, "127.") {
		if echo, err := readEcho(net.JoinHostPort("127.0.0.1", strconv.Itoa(hostPort))); err == nil && echo == "xy" {
			t.Fatalf("restart retargeted hostPort at %s (trap hits %d)", dest, trap.hits.Load())
		}
	}
	if trap.hits.Load() != 0 {
		t.Fatalf("host loopback accepted %d connection(s) after restart", trap.hits.Load())
	}
}

type echoTrap struct {
	ln   net.Listener
	port int
	hits atomic.Int32
}

func listenEcho(t *testing.T) *echoTrap {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tr := &echoTrap{ln: ln, port: ln.Addr().(*net.TCPAddr).Port}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			tr.hits.Add(1)
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return tr
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

func newHostPortEngine(t *testing.T, rt runtime.Runtime) *Engine {
	t.Helper()
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func readEcho(addr string) (string, error) {
	c, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(time.Second))
	if _, err := c.Write([]byte("xy")); err != nil {
		return "", err
	}
	buf := make([]byte, 2)
	n, err := c.Read(buf)
	if err != nil {
		return "", err
	}
	return string(buf[:n]), nil
}
