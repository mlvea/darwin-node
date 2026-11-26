package guest

import (
	"context"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
)

// manualIdleClock is the test clock. Advance moves it and runs timers
// whose deadline has passed, on the caller, so a test can expire an idle
// connection without sleeping. onFire runs on the goroutine that runs the
// timer callback.
type manualIdleClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*manualIdleTimer
	onFire func()
}

type manualIdleTimer struct {
	clk     *manualIdleClock
	when    time.Time
	fn      func()
	stopped bool
}

func newManualIdleClock(now time.Time) *manualIdleClock {
	return &manualIdleClock{now: now}
}

func (c *manualIdleClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualIdleClock) AfterFunc(d time.Duration, fn func()) idleTimer {
	if d < 0 {
		d = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualIdleTimer{
		clk:  c,
		when: c.now.Add(d),
		fn: func() {
			if c.onFire != nil {
				c.onFire()
			}
			fn()
		},
	}
	c.timers = append(c.timers, t)
	return t
}

func (t *manualIdleTimer) Reset(d time.Duration) {
	if d < 0 {
		d = 0
	}
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	t.stopped = false
	t.when = t.clk.now.Add(d)
}

func (t *manualIdleTimer) Stop() {
	t.clk.mu.Lock()
	defer t.clk.mu.Unlock()
	t.stopped = true
}

// Advance moves the clock forward and runs due callbacks. Callbacks run
// without the clock lock so they can Reset timers.
func (c *manualIdleClock) Advance(d time.Duration) {
	if d < 0 {
		d = 0
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var fire []func()
	for _, t := range c.timers {
		if t.stopped || t.when.After(now) {
			continue
		}
		t.stopped = true
		fire = append(fire, t.fn)
	}
	c.mu.Unlock()
	for _, fn := range fire {
		fn()
	}
}

// guestPipe is one in-process agent session with the host heartbeat off,
// so only the test's own frames refresh the idle deadline.
type guestPipe struct {
	h      *Handler
	cli    *Client
	cancel context.CancelFunc
	done   <-chan struct{}
}

func startGuest(t *testing.T, h Handler) *guestPipe {
	t.Helper()
	h.Init()
	agent, host := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = Serve(ctx, agent, h)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		_ = host.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("Serve did not return")
		}
	})
	cli, err := Dial(context.Background(), host, h.Token, "host", HostConfig{HeartbeatInterval: -1})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return &guestPipe{h: &h, cli: cli, cancel: cancel, done: done}
}

func waitServe(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve still running")
	}
}

func rejectWatchdogCallback(t *testing.T) func() {
	t.Helper()
	return func() {
		buf := make([]byte, 8192)
		n := runtime.Stack(buf, false)
		if strings.Contains(string(buf[:n]), "idleWatchdog") {
			t.Errorf("idle deadline ran on the watchdog goroutine\n%s", buf[:n])
		}
	}
}

func TestIdleTrackClosesAtDeadline(t *testing.T) {
	clk := newManualIdleClock(time.Unix(1_700_000_000, 0))
	var live atomic.Int32
	var closed atomic.Int32
	tr := &idleTrack{
		clk:     clk,
		idle:    time.Second,
		live:    live.Load,
		closeFn: func() { closed.Add(1) },
	}
	tr.touch()
	clk.Advance(time.Second - time.Nanosecond)
	if closed.Load() != 0 {
		t.Fatalf("closed %d times before the deadline", closed.Load())
	}
	clk.Advance(time.Nanosecond)
	if closed.Load() != 1 {
		t.Fatalf("closed %d times at the deadline", closed.Load())
	}
	clk.Advance(time.Second)
	if closed.Load() != 1 {
		t.Fatalf("closed %d times after the deadline", closed.Load())
	}
}

func TestIdleTrackTouchPushesDeadline(t *testing.T) {
	clk := newManualIdleClock(time.Unix(1_700_000_000, 0))
	var live atomic.Int32
	var closed atomic.Int32
	tr := &idleTrack{
		clk:     clk,
		idle:    time.Second,
		live:    live.Load,
		closeFn: func() { closed.Add(1) },
	}
	tr.touch()
	clk.Advance(time.Second - time.Millisecond)
	tr.touch()
	clk.Advance(time.Second - time.Millisecond)
	if closed.Load() != 0 {
		t.Fatal("touch did not push the deadline")
	}
	clk.Advance(2 * time.Millisecond)
	if closed.Load() != 1 {
		t.Fatalf("closed %d times after the refreshed deadline", closed.Load())
	}
}

func TestIdleTrackHoldsForInteractive(t *testing.T) {
	clk := newManualIdleClock(time.Unix(1_700_000_000, 0))
	var live atomic.Int32
	live.Store(1)
	var closed atomic.Int32
	tr := &idleTrack{
		clk:     clk,
		idle:    time.Second,
		live:    live.Load,
		closeFn: func() { closed.Add(1) },
	}
	tr.touch()
	clk.Advance(2 * time.Second)
	if closed.Load() != 0 {
		t.Fatal("closed an interactive session")
	}
	live.Store(0)
	clk.Advance(idleInteractiveRecheck)
	if closed.Load() != 1 {
		t.Fatalf("closed %d times after the interactive session ended", closed.Load())
	}
}

func TestWallIdleTimerReset(t *testing.T) {
	fired := make(chan struct{})
	var once sync.Once
	timer := wallIdleClock{}.AfterFunc(time.Hour, func() {
		once.Do(func() { close(fired) })
	})
	defer timer.Stop()
	timer.Reset(20 * time.Millisecond)
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("reset wall idle timer did not fire")
	}
}

// TestActiveConnRefreshesIdleDeadline is the macOS failure of a 150ms idle
// window against a 40ms Health loop: a slow runner let the gap between
// reads pass the window and the pipe closed. The clock moves only when
// the test says so. Heartbeats are off so a wall-clock probe cannot
// refresh the deadline behind the test's back.
func TestActiveConnRefreshesIdleDeadline(t *testing.T) {
	leakcheck.Check(t)
	const idle = time.Second
	clk := newManualIdleClock(time.Unix(1_700_000_000, 0))
	clk.onFire = rejectWatchdogCallback(t)
	h := Handler{Token: "secret", IdleTimeout: idle, clock: clk}
	s := startGuest(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if _, err := s.cli.Health(ctx); err != nil {
		t.Fatal(err)
	}
	clk.Advance(idle - time.Millisecond)
	if res, err := s.cli.Health(ctx); err != nil || !res.OK {
		t.Fatalf("health inside idle window: %v %+v", err, res)
	}
	clk.Advance(idle - time.Millisecond)
	if res, err := s.cli.Health(ctx); err != nil || !res.OK {
		t.Fatalf("active call did not refresh idle deadline: %v %+v", err, res)
	}
	clk.Advance(idle + time.Millisecond)
	if _, err := s.cli.Health(ctx); err == nil {
		t.Fatal("connection stayed open after an idle window with no frames")
	}
}

// TestInteractiveSessionSurvivesIdleClock advances the idle clock past the
// window while an exec is in progress. A silent shell must stay up.
// TestIdleTrackHoldsForInteractive covers the close after that hold drops.
// Checking it here would race the host window update for the exit frame.
func TestInteractiveSessionSurvivesIdleClock(t *testing.T) {
	leakcheck.Check(t)
	const idle = time.Second
	clk := newManualIdleClock(time.Unix(1_700_000_000, 0))
	clk.onFire = rejectWatchdogCallback(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := Handler{
		Token:       "secret",
		IdleTimeout: idle,
		clock:       clk,
		ExecFn: func(ctx context.Context, req ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			once.Do(func() { close(started) })
			select {
			case <-release:
				return 0, nil
			case <-ctx.Done():
				return 1, ctx.Err()
			}
		},
	}
	s := startGuest(t, h)
	execDone := make(chan error, 1)
	go func() {
		// TTY with no stdin, so the only frames before Health are the
		// exec request itself.
		_, err := s.cli.ExecInteractive(context.Background(),
			ExecReq{Argv: []string{"hold"}, TTY: true},
			nil, nil, io.Discard, io.Discard)
		execDone <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("exec did not start")
	}
	clk.Advance(2 * idle)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.cli.Health(ctx); err != nil {
		t.Fatalf("interactive session closed when the idle clock advanced: %v", err)
	}
	close(release)
	select {
	case err := <-execDone:
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec did not finish")
	}
}

// TestIdleWatchdogJoinedOnShutdown waits on Serve's return, which joins
// the watchdog. A sleep would race the goroutine the macOS leakcheck
// reported still inside idleWatchdog.
func TestIdleWatchdogJoinedOnShutdown(t *testing.T) {
	leakcheck.Check(t)
	tests := []struct {
		name string
		stop func(s *guestPipe)
	}{
		{name: "client close", stop: func(s *guestPipe) { _ = s.cli.Close() }},
		{name: "context cancel", stop: func(s *guestPipe) { s.cancel() }},
		{name: "handler close", stop: func(s *guestPipe) { s.h.Close() }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			leakcheck.Check(t)
			h := Handler{Token: "tok", IdleTimeout: time.Hour}
			s := startGuest(t, h)
			tc.stop(s)
			waitServe(t, s.done)
		})
	}
}
