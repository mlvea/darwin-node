package engine

import (
	"sync"
	"time"
)

// VirtualClock is a probe clock that does not sleep. Advance moves Now and
// fires timers whose deadline has passed. The lifecycle fuzzer installs one
// so a start retry's backoff does not occupy wall time. A nil engine clock
// remains the wall clock; production does not construct a VirtualClock.
type VirtualClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*virtualTimer
}

// virtualTimer is one wait armed by VirtualClock.NewTimer.
type virtualTimer struct {
	c       *VirtualClock
	ch      chan time.Time
	when    time.Time
	stopped bool
}

// NewVirtualClock starts at now. A zero now is Unix epoch.
func NewVirtualClock(now time.Time) *VirtualClock {
	if now.IsZero() {
		now = time.Unix(0, 0).UTC()
	}
	return &VirtualClock{now: now}
}

// Now is the logical time.
func (c *VirtualClock) Now() time.Time {
	if c == nil {
		return time.Unix(0, 0).UTC()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// NewTimer arms a probe wait. A non-positive duration fires immediately.
func (c *VirtualClock) NewTimer(d time.Duration) probeTimer {
	if c == nil {
		return wallClock{}.NewTimer(d)
	}
	if d < 0 {
		d = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &virtualTimer{c: c, ch: make(chan time.Time, 1), when: c.now.Add(d)}
	c.waiters = append(c.waiters, t)
	if d == 0 {
		t.stopped = true
		t.ch <- c.now
	}
	return t
}

func (t *virtualTimer) C() <-chan time.Time { return t.ch }

func (t *virtualTimer) Stop() bool {
	if t == nil || t.c == nil {
		return false
	}
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

// Advance moves logical time forward by d and delivers every timer that is
// due. d <= 0 does not move the clock. The send happens without the lock so
// a waiter can call back into the clock.
func (c *VirtualClock) Advance(d time.Duration) {
	if c == nil || d <= 0 {
		return
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var fire []chan time.Time
	keep := c.waiters[:0]
	for _, t := range c.waiters {
		if t.stopped {
			continue
		}
		if !t.when.After(now) {
			t.stopped = true
			fire = append(fire, t.ch)
			continue
		}
		keep = append(keep, t)
	}
	c.waiters = keep
	c.mu.Unlock()
	for _, ch := range fire {
		select {
		case ch <- now:
		default:
		}
	}
}

// SetVirtualClockForTest installs c as the probe and start-backoff clock.
// Nil restores the wall clock. Call it before any pod start. Not a process flag.
func (e *Engine) SetVirtualClockForTest(c *VirtualClock) {
	if e == nil {
		return
	}
	if c == nil {
		e.clock = nil
		return
	}
	e.clock = c
}
