package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// stopGateRuntime fails Start so the pod is Failed, lets the failure path's
// Stop finish, then blocks the Stop Delete uses so the test can read status.
type stopGateRuntime struct {
	runtime.Runtime
	stops     atomic.Int32
	firstDone chan struct{}
	entered   chan struct{}
	hold      chan struct{}
	onceFirst sync.Once
	onceNext  sync.Once
}

func (r *stopGateRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &stopGateMachine{Machine: m, rt: r}, nil
}

type stopGateMachine struct {
	runtime.Machine
	rt *stopGateRuntime
}

func (m *stopGateMachine) Start(context.Context) error {
	return errors.New("injected start failure")
}

func (m *stopGateMachine) Stop(ctx context.Context, grace time.Duration) error {
	n := m.rt.stops.Add(1)
	if n == 1 {
		err := m.Machine.Stop(ctx, grace)
		m.rt.onceFirst.Do(func() { close(m.rt.firstDone) })
		return err
	}
	m.rt.onceNext.Do(func() { close(m.rt.entered) })
	<-m.rt.hold
	return m.Machine.Stop(ctx, grace)
}

func TestDeleteKeepsFailedPhase(t *testing.T) {
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
	rt := &stopGateRuntime{
		Runtime:   fake.New(),
		firstDone: make(chan struct{}),
		entered:   make(chan struct{}),
		hold:      make(chan struct{}),
	}
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	var once sync.Once
	release := func() { once.Do(func() { close(rt.hold) }) }
	var wg sync.WaitGroup
	defer func() {
		release()
		wg.Wait()
		e.Close()
	}()

	if err := e.Create(context.Background(), samplePod("failed", "uid-failed"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "failed", corev1.PodFailed)
	select {
	case <-rt.firstDone:
	case <-time.After(5 * time.Second):
		t.Fatal("failure path did not stop the VM")
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = e.Delete(context.Background(), "default", "failed", 0)
	}()
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("delete did not reach Stop")
	}
	p, err := e.Get("default", "failed")
	if err != nil {
		t.Fatal(err)
	}
	phase := p.Status.Phase
	exit := int32(-1)
	if len(p.Status.ContainerStatuses) > 0 && p.Status.ContainerStatuses[0].State.Terminated != nil {
		exit = p.Status.ContainerStatuses[0].State.Terminated.ExitCode
	}
	release()
	wg.Wait()
	if phase != corev1.PodFailed || exit != 1 {
		t.Fatalf("phase %s exit %d, failed pod was rewritten during delete", phase, exit)
	}
}
