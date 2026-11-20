package engine

import "time"

// probeClock schedules probe waits. A nil Engine clock is the wall clock.
// Tests install a clock they can move so a period elapses without sleeping.
type probeClock interface {
	Now() time.Time
	NewTimer(d time.Duration) probeTimer
}

// probeTimer is one wait. Stop releases it. The channel delivers one time.
type probeTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) NewTimer(d time.Duration) probeTimer {
	if d < 0 {
		d = 0
	}
	return wallTimer{t: time.NewTimer(d)}
}

type wallTimer struct{ t *time.Timer }

func (w wallTimer) C() <-chan time.Time { return w.t.C }

func (w wallTimer) Stop() bool { return w.t.Stop() }

func (e *Engine) now() time.Time {
	if e == nil || e.clock == nil {
		return time.Now()
	}
	return e.clock.Now()
}

func (e *Engine) newProbeTimer(d time.Duration) probeTimer {
	if e == nil || e.clock == nil {
		return wallClock{}.NewTimer(d)
	}
	return e.clock.NewTimer(d)
}
