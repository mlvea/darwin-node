package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/types"

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
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// One transient failure used to fail the pod. The start is retried, so the
// pod reaches Running and keeps its slot.
func waitRunningOrFail(t *testing.T, e *Engine, ns, name string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get(ns, name)
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("transient error failed pod %s: %s", name, p.Status.Message)
		}
		if err == nil && p.Status.Phase == corev1.PodRunning {
			if e.slots.Used() != 1 {
				t.Fatalf("running pod holds %d slots", e.slots.Used())
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ := e.Get(ns, name)
	t.Fatalf("pod %s did not run: %+v", name, p.Status)
}

func TestRuntimeCreateFailureIsRetried(t *testing.T) {
	e := injectEngine(t, failCreate)
	if err := e.Create(context.Background(), samplePod("fc", "uid-fc"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitRunningOrFail(t, e, "default", "fc")
	_ = e.Delete(context.Background(), "default", "fc", 0)
}

func TestVMStartFailureIsRetried(t *testing.T) {
	e := injectEngine(t, failStart)
	if err := e.Create(context.Background(), samplePod("fs", "uid-fs"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitRunningOrFail(t, e, "default", "fs")
	_ = e.Delete(context.Background(), "default", "fs", 0)
}

func TestAgentDialFailureIsRetried(t *testing.T) {
	e := injectEngine(t, failDial)
	pod := samplePod("fd", "uid-fd")
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitRunningOrFail(t, e, "default", "fd")
	_ = e.Delete(context.Background(), "default", "fd", 0)
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
	t.Cleanup(func() { _ = e.Close() })

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
