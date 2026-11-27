package simvz

import (
	"context"
	"fmt"
	"math/rand/v2"
	"runtime"
	"sync"
	"time"
)

const (
	// OrderKVOBeforeCompletion delivers guestDidStop, then KVO Stopped,
	// then the completion handler.
	OrderKVOBeforeCompletion = "kvo_before_stop_completion"
	// OrderCompletionBeforeKVO delivers guestDidStop, then the completion
	// handler, then KVO Stopped.
	OrderCompletionBeforeKVO = "stop_completion_before_kvo"
	// OrderJitter permutes those three. OrderSeed reproduces it.
	OrderJitter = "jitter"
)

type stopStep int

const (
	stepGuestStop stopStep = iota
	stepKVOStopped
	stepStopDone
)

// gate is one armed Barrier. The actor closes entered when it arrives
// and waits until ReleaseBarrier closes release.
type gate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	relOnce sync.Once
}

func (r *Runtime) initOrder(opts Options) {
	r.order = opts.Order
	seed := opts.OrderSeed
	if r.order == OrderJitter && seed == 0 {
		seed = time.Now().UnixNano()
		if seed == 0 {
			seed = 1
		}
	}
	r.orderSeed = seed
	if seed != 0 {
		r.rng = rand.New(rand.NewPCG(uint64(seed), 0x565A434F4E))
	}
}

// OrderSeed is the jitter seed in use. A failing test prints it.
func (r *Runtime) OrderSeed() int64 {
	if r == nil {
		return 0
	}
	return r.orderSeed
}

// ArmBarrier makes the next barrier of this name block until
// ReleaseBarrier. Most barriers run on the per-VM actor.
// before_accept_wait runs on the Stop caller, after Stopped is published.
// Arm it before the operation that hits the name.
func (r *Runtime) ArmBarrier(name string) {
	if r == nil || name == "" {
		return
	}
	r.gateMu.Lock()
	defer r.gateMu.Unlock()
	if r.gates == nil {
		r.gates = map[string]*gate{}
	}
	r.gates[name] = &gate{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// WaitBarrier blocks until the actor arrives at an armed barrier.
func (r *Runtime) WaitBarrier(ctx context.Context, name string) error {
	if r == nil {
		return fmt.Errorf("simvz: nil runtime")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.gateMu.Lock()
	g := r.gates[name]
	r.gateMu.Unlock()
	if g == nil {
		return fmt.Errorf("simvz: barrier %q is not armed", name)
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.entered:
		return nil
	}
}

// ReleaseBarrier unblocks the actor waiting in Barrier.
func (r *Runtime) ReleaseBarrier(name string) {
	if r == nil {
		return
	}
	r.gateMu.Lock()
	g := r.gates[name]
	r.gateMu.Unlock()
	if g == nil {
		return
	}
	g.relOnce.Do(func() { close(g.release) })
}

func (m *machine) barrier(ctx context.Context, name string) {
	if m == nil || m.rt == nil {
		return
	}
	m.rt.hitBarrier(ctx, name)
}

func (r *Runtime) hitBarrier(ctx context.Context, name string) {
	r.jitter(ctx)
	r.gateMu.Lock()
	var g *gate
	if r.gates != nil {
		g = r.gates[name]
	}
	r.gateMu.Unlock()
	if g == nil {
		return
	}
	g.once.Do(func() { close(g.entered) })
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
	case <-g.release:
	}
}

// jitter burns 0-5 Gosched yields and 0-500us when Order is jitter.
func (r *Runtime) jitter(ctx context.Context) {
	if r == nil || r.order != OrderJitter || r.rng == nil {
		return
	}
	r.rngMu.Lock()
	n := r.rng.IntN(6)
	us := r.rng.IntN(501)
	r.rngMu.Unlock()
	for i := 0; i < n; i++ {
		runtime.Gosched()
	}
	if us == 0 {
		return
	}
	t := time.NewTimer(time.Duration(us) * time.Microsecond)
	defer t.Stop()
	if ctx == nil {
		<-t.C
		return
	}
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (r *Runtime) stopSteps() []stopStep {
	base := []stopStep{stepGuestStop, stepKVOStopped, stepStopDone}
	if r == nil {
		return base
	}
	switch r.order {
	case OrderCompletionBeforeKVO:
		return []stopStep{stepGuestStop, stepStopDone, stepKVOStopped}
	case OrderJitter:
		r.rngMu.Lock()
		defer r.rngMu.Unlock()
		if r.rng == nil {
			return base
		}
		out := append([]stopStep(nil), base...)
		r.rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	default:
		return base
	}
}

func (r *Runtime) completionBeforeKVO() bool {
	if r == nil {
		return false
	}
	switch r.order {
	case OrderCompletionBeforeKVO:
		return true
	case OrderJitter:
		r.rngMu.Lock()
		defer r.rngMu.Unlock()
		if r.rng == nil {
			return false
		}
		return r.rng.IntN(2) == 1
	default:
		return false
	}
}
