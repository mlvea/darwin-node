package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

// Failure injection: every error path must fail the pod closed, release its
// slot, reclaim its on-disk state, and leave no goroutines behind.

type failMode int

const (
	failNone failMode = iota
	failCreate
	failStart
	failDial
)

var errInjected = errors.New("injected failure")

// injectRuntime fails the first `times` operations of its kind, then passes
// through, so tests can prove the node recovers after a failure.
type injectRuntime struct {
	runtime.Runtime
	fail  failMode
	times int
}

func (r *injectRuntime) take() bool {
	if r.times <= 0 {
		return false
	}
	r.times--
	return true
}

func (r *injectRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	if r.fail == failCreate && r.take() {
		return nil, errInjected
	}
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	if r.fail != failNone {
		return &injectMachine{Machine: m, rt: r}, nil
	}
	return m, nil
}

type injectMachine struct {
	runtime.Machine
	rt *injectRuntime
}

func (m *injectMachine) Start(ctx context.Context) error {
	if m.rt.fail == failStart && m.rt.take() {
		return errInjected
	}
	return m.Machine.Start(ctx)
}

func (m *injectMachine) DialAgent(ctx context.Context) (*guest.Client, error) {
	if m.rt.fail == failDial && m.rt.take() {
		return nil, errInjected
	}
	return m.Machine.DialAgent(ctx)
}

// Status hides the fake's default IP in dial-failure mode so the host-side
// fallback fails fast instead of waiting out a 45s NAT dial.
func (m *injectMachine) Status() types.VMStatus {
	if m.rt.fail == failDial {
		return types.VMStatus{State: types.VMRunning}
	}
	return m.Machine.Status()
}

func injectEngine(t *testing.T, fail failMode) *Engine {
	t.Helper()
	leakcheck.Check(t)
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 2 * time.Second
	cfg.AllowNATWorkloads = true
	inner := fake.New()
	if fail == failDial {
		inner.IP = "" // force the fast no-IP fallback instead of a 45s NAT wait
	}
	rt := &injectRuntime{Runtime: inner, fail: fail, times: 1}
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)
	return e
}

func assertFailedAndClean(t *testing.T, e *Engine, ns, name, uid string) {
	t.Helper()
	p := waitPhase(t, e, ns, name, corev1.PodFailed)
	if p.Status.Message == "" {
		t.Fatal("failed pod must carry a message")
	}
	if got := e.slots.Used(); got != 0 {
		t.Fatalf("slot leak after failure: used=%d", got)
	}
	dir := filepath.Join(e.cfg.CacheDir, "pods", uid)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("failed pod left state behind: %v", err)
	}
	// The node must be immediately reusable.
	next := samplePod(name+"-next", uid+"-next")
	if err := e.Create(context.Background(), next, Credentials{}); err != nil {
		t.Fatalf("node not reusable after failure: %v", err)
	}
	waitPhase(t, e, ns, name+"-next", corev1.PodRunning)
	_ = e.Delete(context.Background(), ns, name+"-next", 0)
}

func TestRuntimeCreateFailureFailsClosed(t *testing.T) {
	e := injectEngine(t, failCreate)
	if err := e.Create(context.Background(), samplePod("fc", "uid-fc"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	assertFailedAndClean(t, e, "default", "fc", "uid-fc")
}

func TestVMStartFailureFailsClosed(t *testing.T) {
	e := injectEngine(t, failStart)
	if err := e.Create(context.Background(), samplePod("fs", "uid-fs"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	assertFailedAndClean(t, e, "default", "fs", "uid-fs")
}

func TestAgentDialFailureFailsClosed(t *testing.T) {
	e := injectEngine(t, failDial)
	pod := samplePod("fd", "uid-fd")
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	assertFailedAndClean(t, e, "default", "fd", "uid-fd")
}

// Warm boots share the same runtime; their failures must clean slots and
// directories too.
func TestWarmBootFailureCleansUp(t *testing.T) {
	leakcheck.Check(t)
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = time.Second
	cfg.AllowNATWorkloads = true
	cfg.WarmSlots = 1
	cfg.WarmImage = warmImg
	e := New(cfg, slots, &injectRuntime{Runtime: fake.New(), fail: failCreate, times: 5},
		sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)

	// Give the one-second replenisher a couple of ticks to fail.
	time.Sleep(1600 * time.Millisecond)
	if n := e.warm.len(); n != 0 {
		t.Fatalf("warm entries survived boot failures: %d", n)
	}
	if used := e.slots.Used(); used != 0 {
		t.Fatalf("warm failures leaked slots: %d", used)
	}
	warmRoot := filepath.Join(cfg.CacheDir, "warm")
	if ents, _ := os.ReadDir(warmRoot); len(ents) != 0 {
		t.Fatalf("warm root not cleaned: %d entries", len(ents))
	}
}
