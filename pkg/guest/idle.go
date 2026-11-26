package guest

import (
	"context"
	"io"
	"sync"
	"time"
)

// idleClock is the idle deadline's time source. A nil Handler clock uses
// the wall clock. Tests inject a clock so an idle window can elapse
// without sleeping. IdleMS on the wire stays the configured duration.
type idleClock interface {
	Now() time.Time
	AfterFunc(d time.Duration, fn func()) idleTimer
}

// idleTimer is one armed idle deadline.
type idleTimer interface {
	Reset(d time.Duration)
	Stop()
}

// idleInteractiveRecheck is how often a silent exec is looked at again so
// the connection can close once the exec ends. It does not extend the
// idle deadline. The previous watchdog polled at this rate.
const idleInteractiveRecheck = 250 * time.Millisecond

// idleTrack closes the connection when no frame has been accepted for the
// idle window and no interactive stream is running. touch is safe from
// the read loop.
type idleTrack struct {
	mu      sync.Mutex
	clk     idleClock
	idle    time.Duration
	live    func() int32
	closeFn func()
	last    time.Time
	timer   idleTimer
	stopped bool
}

func newIdleTrack(h Handler, rw io.ReadWriteCloser, conn *connState) *idleTrack {
	clk := h.clock
	if clk == nil {
		clk = wallIdleClock{}
	}
	t := &idleTrack{
		clk:     clk,
		idle:    h.idleTimeout(),
		closeFn: func() { _ = rw.Close() },
	}
	if conn != nil {
		t.live = conn.liveInteractive.Load
	}
	t.touch()
	return t
}

// touch pushes the deadline out to one idle window from now.
func (t *idleTrack) touch() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.idle <= 0 {
		return
	}
	t.last = t.clk.Now()
	t.armLocked(t.idle)
}

// fire runs on the clock's timer, not on the watchdog goroutine. The
// watchdog only waits for shutdown, so a stack sample of a live
// connection always shows that one select. A poll inside the goroutine
// changed its source line between samples and leakcheck reported the
// same goroutine as a new leak.
func (t *idleTrack) fire() {
	t.mu.Lock()
	if t.stopped || t.idle <= 0 {
		t.mu.Unlock()
		return
	}
	if t.live != nil && t.live() > 0 {
		t.armLocked(idleInteractiveRecheck)
		t.mu.Unlock()
		return
	}
	elapsed := t.clk.Now().Sub(t.last)
	if elapsed < t.idle {
		t.armLocked(t.idle - elapsed)
		t.mu.Unlock()
		return
	}
	t.stopped = true
	closeFn := t.closeFn
	t.mu.Unlock()
	if closeFn != nil {
		closeFn()
	}
}

func (t *idleTrack) armLocked(d time.Duration) {
	if d < 0 {
		d = 0
	}
	if t.timer == nil {
		t.timer = t.clk.AfterFunc(d, t.fire)
		return
	}
	t.timer.Reset(d)
}

func (t *idleTrack) stop() {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	if t.timer != nil {
		t.timer.Stop()
	}
}

// idleWatchdog waits until Serve is done, the handler is closed, or ctx
// is cancelled, then unblocks the read loop. It does not decide whether
// the connection is idle; idleTrack's timer does.
func (h *Handler) idleWatchdog(ctx context.Context, rw io.ReadWriteCloser, done <-chan struct{}) {
	var shut <-chan struct{}
	if h.live != nil {
		shut = h.live.shut
	}
	select {
	case <-ctx.Done():
		// Unblock Serve's read. Cancelling ctx alone left the connection
		// parked until IdleTimeout.
		_ = rw.Close()
	case <-done:
	case <-shut:
		// Handler.Close. The host may still hold the client, so ctx stays
		// active and the idle window has not elapsed.
		_ = rw.Close()
	}
}

type wallIdleClock struct{}

func (wallIdleClock) Now() time.Time { return time.Now() }

func (wallIdleClock) AfterFunc(d time.Duration, fn func()) idleTimer {
	if d < 0 {
		d = 0
	}
	return wallIdleTimer{t: time.AfterFunc(d, fn)}
}

type wallIdleTimer struct{ t *time.Timer }

func (w wallIdleTimer) Reset(d time.Duration) {
	if d < 0 {
		d = 0
	}
	w.t.Reset(d)
}

func (w wallIdleTimer) Stop() { w.t.Stop() }
