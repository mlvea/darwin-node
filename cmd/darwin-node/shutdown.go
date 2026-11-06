package main

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/darwin-node/darwin-node/pkg/engine"
)

// shutdownGate runs the first-signal drain and the second-signal force stop.
//
// Graceful is what run calls after the first SIGINT/SIGTERM cancels the
// process context: drain for the budget, then Engine.Close. Force is the
// second signal. It cancels the drain so the budget is not waited out, and
// it does not return until Close has stopped whatever pods remain. Graceful
// then blocks on Release. The process path exits 130 from the signal
// goroutine and does not call Release, so main cannot return 0 first.
type shutdownGate struct {
	mu          sync.Mutex
	eng         *engine.Engine
	started     bool
	forced      bool
	cancelDrain context.CancelFunc
	finished    chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
}

func newShutdownGate() *shutdownGate {
	return &shutdownGate{
		finished: make(chan struct{}),
		release:  make(chan struct{}),
	}
}

func (g *shutdownGate) bind(eng *engine.Engine) {
	g.mu.Lock()
	g.eng = eng
	g.mu.Unlock()
}

func (g *shutdownGate) Forced() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.forced
}

// Release unblocks Graceful after a forced stop. Tests call it from the
// exit hook. os.Exit does not return, so the process path never calls it.
func (g *shutdownGate) Release() {
	g.releaseOnce.Do(func() { close(g.release) })
}

// Graceful drains for grace, then Close. A non-positive grace skips the
// drain and still Close's, matching --shutdown-grace-period=0. A second
// call waits until the first has Closed.
func (g *shutdownGate) Graceful(grace time.Duration) {
	g.mu.Lock()
	if g.started {
		g.mu.Unlock()
		<-g.finished
		g.holdIfForced()
		return
	}
	g.started = true
	eng := g.eng
	skipDrain := g.forced || eng == nil || grace <= 0
	var drainCtx context.Context
	if !skipDrain {
		var cancel context.CancelFunc
		drainCtx, cancel = context.WithTimeout(context.Background(), grace)
		g.cancelDrain = cancel
	}
	g.mu.Unlock()

	if drainCtx != nil {
		if err := eng.Drain(drainCtx); err != nil {
			slog.Warn("drain incomplete", "error", err)
		}
		g.mu.Lock()
		if g.cancelDrain != nil {
			g.cancelDrain()
			g.cancelDrain = nil
		}
		g.mu.Unlock()
	}
	if eng != nil {
		eng.Close()
	}
	close(g.finished)
	g.holdIfForced()
}

func (g *shutdownGate) holdIfForced() {
	g.mu.Lock()
	forced := g.forced
	g.mu.Unlock()
	if forced {
		<-g.release
	}
}

// Force skips the rest of the drain. It returns only after Close.
func (g *shutdownGate) Force() {
	g.mu.Lock()
	g.forced = true
	if g.cancelDrain != nil {
		g.cancelDrain()
	}
	if g.started {
		g.mu.Unlock()
		<-g.finished
		return
	}
	g.started = true
	eng := g.eng
	g.mu.Unlock()
	if eng != nil {
		eng.Close()
	}
	close(g.finished)
}

// watchSecondSignal waits until ctx is cancelled (the first SIGINT or
// SIGTERM), then for one more of those signals. force runs to completion
// before exit so the second signal still Close's instead of abandoning VMs.
func watchSecondSignal(ctx context.Context, arm func() <-chan os.Signal, exit func(int), force func()) {
	<-ctx.Done()
	<-arm()
	if force != nil {
		force()
	}
	exit(130)
}
