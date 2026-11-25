package main

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/engine"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/types"
)

// bootThenHold starts the fake VM and then blocks so Close can return
// while the machine is already running.
type bootThenHold struct {
	runtime.Runtime
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *bootThenHold) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := h.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &bootThenHoldMachine{Machine: m, rt: h}, nil
}

type bootThenHoldMachine struct {
	runtime.Machine
	rt *bootThenHold
}

func (m *bootThenHoldMachine) Start(ctx context.Context) error {
	if err := m.Machine.Start(ctx); err != nil {
		return err
	}
	m.rt.once.Do(func() {
		close(m.rt.entered)
		<-m.rt.release
	})
	return nil
}

// TestGracefulReapsBootAfterCloseDeadline is the shutdown path. Graceful
// has to stay up after Close's deadline long enough to stop a VM whose
// Start then returns.
func TestGracefulReapsBootAfterCloseDeadline(t *testing.T) {
	leakcheck.Check(t)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)

	inner := fake.New()
	rt := &bootThenHold{Runtime: inner, entered: make(chan struct{}), release: release}
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = types.RuntimeFake
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 2 * time.Second
	cfg.AllowNATWorkloads = true
	eng := engine.New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	eng.SetShutdownBudgetsForTest(200*time.Millisecond, 3*time.Second)
	t.Cleanup(func() { _ = eng.Close() })

	if err := eng.Create(context.Background(), signalPod("a", "uid-grace-reap"), engine.Credentials{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Start was not reached")
	}

	gate := newShutdownGate()
	gate.bind(eng)
	done := make(chan struct{})
	go func() {
		gate.Graceful(0)
		close(done)
	}()
	t.Cleanup(func() {
		unblock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	select {
	case <-done:
		t.Fatalf("Graceful returned before Start, running=%d", inner.Running())
	case <-time.After(800 * time.Millisecond):
	}
	unblock()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Graceful did not return after Start")
	}
	if inner.Running() != 0 || eng.Slots().Used() != 0 {
		t.Fatalf("running=%d slots=%d after Graceful", inner.Running(), eng.Slots().Used())
	}
}
