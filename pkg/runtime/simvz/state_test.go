package simvz

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/types"
)

// gateClock blocks the first positive Sleep until Release. Later sleeps
// return immediately so Stop can finish after the state under test was seen.
type gateClock struct {
	ch   chan struct{}
	once sync.Once
}

func newGateClock() *gateClock {
	return &gateClock{ch: make(chan struct{})}
}

func (g *gateClock) Now() time.Time { return time.Unix(0, 0) }

func (g *gateClock) Wall(d time.Duration) time.Duration { return d }

func (g *gateClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.ch:
		return nil
	}
}

func (g *gateClock) Release() { g.once.Do(func() { close(g.ch) }) }

func TestWrongStateAndPauseSave(t *testing.T) {
	before := fdBefore(t)
	rt := newRT(t, Options{})
	m := mustMachine(t, rt, "state")
	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	err := m.Start(ctx)
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second start %v", err)
	}
	var se *StateError
	if !errors.As(err, &se) || se.Op != "start" || se.State != "Running" {
		t.Fatalf("state error %+v", err)
	}
	if err := m.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if m.State() != "Paused" || m.Status().State != types.VMRunning {
		t.Fatalf("paused state %s exposed %s", m.State(), m.Status().State)
	}
	if rt.Running() != 1 {
		t.Fatalf("paused VM dropped its slot: %d", rt.Running())
	}
	if err := m.Start(ctx); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("start while paused %v", err)
	}
	if err := m.Save(ctx); err != nil {
		t.Fatal(err)
	}
	if m.State() != "Stopped" || rt.Running() != 0 {
		t.Fatalf("after save %s running %d", m.State(), rt.Running())
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if err := m.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	if m.State() != "Running" {
		t.Fatalf("resumed %s", m.State())
	}
	if err := m.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.Pause(ctx); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("pause while stopped %v", err)
	}
	fdBack(t, before)
}

func TestStopWhileStartingIsRejected(t *testing.T) {
	g := newGateClock()
	rt := newRT(t, Options{Clock: g, Transition: time.Second})
	m := mustMachine(t, rt, "starting")
	t.Cleanup(g.Release)
	errc := make(chan error, 1)
	go func() { errc <- m.Start(context.Background()) }()
	deadline := time.Now().Add(2 * time.Second)
	for m.State() != "Starting" {
		if time.Now().After(deadline) {
			g.Release()
			t.Fatal("never reached Starting")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.Stop(context.Background(), 0); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("stop while starting %v", err)
	}
	if rt.Running() != 1 {
		t.Fatalf("starting VM should hold a slot, running %d", rt.Running())
	}
	g.Release()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if m.State() != "Running" {
		t.Fatalf("state %s", m.State())
	}
	if err := m.Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
}

// nthClock blocks the blockAt-th positive Sleep until Release.
// Start's transition is sleep 1. Pause's transition is sleep 2.
type nthClock struct {
	blockAt int
	ch      chan struct{}
	once    sync.Once

	mu sync.Mutex
	n  int
}

func (c *nthClock) Now() time.Time { return time.Unix(0, 0) }

func (c *nthClock) Wall(d time.Duration) time.Duration { return d }

func (c *nthClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	c.mu.Lock()
	c.n++
	block := c.n == c.blockAt
	c.mu.Unlock()
	if !block {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ch:
		return nil
	}
}

func (c *nthClock) Release() { c.once.Do(func() { close(c.ch) }) }

func TestPauseIsObservable(t *testing.T) {
	clk := &nthClock{blockAt: 2, ch: make(chan struct{})}
	rt := newRT(t, Options{Clock: clk, Transition: time.Second})
	m := mustMachine(t, rt, "pausing")
	t.Cleanup(clk.Release)
	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	errc := make(chan error, 1)
	go func() { errc <- m.Pause(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for m.State() != "Pausing" {
		if time.Now().After(deadline) {
			clk.Release()
			t.Fatal("never reached Pausing")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := m.Start(ctx); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("start while pausing %v", err)
	}
	clk.Release()
	if err := <-errc; err != nil {
		t.Fatal(err)
	}
	if m.State() != "Paused" {
		t.Fatalf("state %s", m.State())
	}
}
