package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// holdStartRuntime blocks inside Start until release is closed. Create has
// already returned, so the VM slot is held and a boot may still happen.
type holdStartRuntime struct {
	runtime.Runtime
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *holdStartRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := h.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &holdStartMachine{Machine: m, rt: h}, nil
}

type holdStartMachine struct {
	runtime.Machine
	rt *holdStartRuntime
}

func (m *holdStartMachine) Start(ctx context.Context) error {
	m.rt.once.Do(func() {
		close(m.rt.entered)
		<-m.rt.release
	})
	return m.Machine.Start(ctx)
}

// holdRemoveSidecar blocks inside RemovePod. Stop has already run by then,
// and the slot is released only after RemovePod returns.
type holdRemoveSidecar struct {
	sidecar.None
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *holdRemoveSidecar) RemovePod(context.Context, string, string, int64) error {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	return nil
}

func deadlineEngine(t *testing.T, rt runtime.Runtime, sc sidecar.Runtime) *Engine {
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
	cfg.ProbeInterval = 20 * time.Millisecond
	e := New(cfg, slots, rt, sc, event.Nop{}, "10.0.0.1")
	e.closeTimeout = 500 * time.Millisecond
	return e
}

func startClose(e *Engine) (chan struct{}, *error) {
	done := make(chan struct{})
	var err error
	go func() {
		err = e.Close()
		close(done)
	}()
	return done, &err
}

// expectCloseReturns starts Close and reports whether it came back while
// unblock was still held. The slot must still be accounted: releasing it
// would let another pod boot a VM that this call might still start.
func expectCloseReturns(t *testing.T, e *Engine, unblock func(), done <-chan struct{}, closeErr *error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		unblock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Close did not return after the blocked call was released")
		}
		t.Fatal("Close blocked while a VM call had not returned")
	}
	if closeErr == nil || *closeErr == nil || !strings.Contains((*closeErr).Error(), "VM slot kept") {
		got := error(nil)
		if closeErr != nil {
			got = *closeErr
		}
		t.Fatalf("Close error = %v, want the VM slot kept", got)
	}
	if got := e.slots.Used(); got != 1 {
		t.Fatalf("Close released the slot while a VM might still be booting: used=%d", got)
	}
}

func waitSlotReleased(t *testing.T, e *Engine, rt *fake.Runtime) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e.slots.Used() == 0 && rt.Running() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("after the blocked call returned, slots=%d running=%d", e.slots.Used(), rt.Running())
}

func TestCloseDuringStuckCreateKeepsTheSlot(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	inner := fake.New()
	rt := &holdCreateRuntime{Runtime: inner, entered: make(chan struct{}), release: release}
	e := deadlineEngine(t, rt, sidecar.None{})

	if err := e.Create(context.Background(), samplePod("a", "uid-close-create"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Runtime.Create was not reached")
	}

	done, closeErr := startClose(e)
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("Close did not return")
		}
	})
	expectCloseReturns(t, e, unblock, done, closeErr)
	if inner.Running() != 0 {
		t.Fatalf("running=%d before Create returned", inner.Running())
	}
	if err := e.Create(context.Background(), samplePod("b", "uid-close-create-b"), Credentials{}); err == nil {
		t.Fatal("create admitted while a VM might still be booting")
	}
	unblock()
	waitSlotReleased(t, e, inner)
}

func TestCloseDuringStuckStartKeepsTheSlot(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	inner := fake.New()
	rt := &holdStartRuntime{Runtime: inner, entered: make(chan struct{}), release: release}
	e := deadlineEngine(t, rt, sidecar.None{})

	if err := e.Create(context.Background(), samplePod("a", "uid-close-start"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Start was not reached")
	}

	done, closeErr := startClose(e)
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("Close did not return")
		}
	})
	expectCloseReturns(t, e, unblock, done, closeErr)
	if inner.Running() != 0 {
		t.Fatalf("running=%d before Start returned", inner.Running())
	}
	unblock()
	waitSlotReleased(t, e, inner)
}

func TestCloseDuringStuckRemovePodKeepsTheSlot(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	inner := fake.New()
	sc := &holdRemoveSidecar{entered: make(chan struct{}), release: release}
	e := deadlineEngine(t, inner, sc)
	if err := e.Create(context.Background(), samplePod("a", "uid-close-rm"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "a", corev1.PodRunning)

	done, closeErr := startClose(e)
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("Close did not return")
		}
	})
	select {
	case <-sc.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("RemovePod was not reached")
	}
	expectCloseReturns(t, e, unblock, done, closeErr)
	if inner.Running() != 0 {
		t.Fatalf("running=%d, Stop should have finished before RemovePod", inner.Running())
	}
	unblock()
	waitSlotReleased(t, e, inner)
}

func TestCloseDuringStuckWarmStartKeepsTheSlot(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	inner := fake.New()
	rt := &holdStartRuntime{Runtime: inner, entered: make(chan struct{}), release: release}
	e := deadlineEngine(t, rt, sidecar.None{})
	bootDone := make(chan error, 1)
	go func() {
		bootDone <- e.bootWarm(context.Background(), warmImg)
	}()
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("warm Start was not reached")
	}
	if e.slots.Used() != 1 {
		t.Fatalf("warm boot did not hold a slot: used=%d", e.slots.Used())
	}

	done, closeErr := startClose(e)
	t.Cleanup(func() {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("Close did not return")
		}
		select {
		case <-bootDone:
		case <-time.After(5 * time.Second):
			t.Errorf("warm boot did not return")
		}
	})
	expectCloseReturns(t, e, unblock, done, closeErr)
	if inner.Running() != 0 {
		t.Fatalf("running=%d before warm Start returned", inner.Running())
	}
	unblock()
	waitSlotReleased(t, e, inner)
}
