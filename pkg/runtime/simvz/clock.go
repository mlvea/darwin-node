package simvz

import (
	"context"
	"sync"
	"time"
)

// Clock is the time hook fault injection sleeps on. Wall reports how long
// a logical duration occupies on the real clock. A nil clock is the wall
// clock, where Wall is the identity.
//
// The guest agent's idle watchdog (pkg/guest, 250ms poll, 30s default)
// does not use this clock. Callers that scale time also pass the scaled
// idle to the agent so a 35s stall is still longer than a 30s idle without
// sleeping the full 35s. The scale has to leave the wall idle longer than
// that 250ms poll, or the watchdog cannot see it.
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
	Wall(d time.Duration) time.Duration
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

func (wallClock) Wall(d time.Duration) time.Duration { return d }

func (wallClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ScaledClock sleeps d/Scale and then reports Now advanced by d.
// Scale 10 makes a 30s idle 3s of wall time. Scale below 1 is treated as 1.
type ScaledClock struct {
	Scale int

	mu  sync.Mutex
	now time.Time
}

// NewScaledClock returns a clock whose logical epoch is the wall clock at
// construction. Sleep moves Now forward by the logical duration.
func NewScaledClock(scale int) *ScaledClock {
	if scale < 1 {
		scale = 1
	}
	return &ScaledClock{Scale: scale, now: time.Now()}
}

// Now is the logical time.
func (c *ScaledClock) Now() time.Time {
	if c == nil {
		return time.Now()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Wall converts a logical duration to the real sleep. A positive duration
// that would round to zero sleeps one nanosecond so a fault cannot vanish.
func (c *ScaledClock) Wall(d time.Duration) time.Duration {
	if c == nil || c.Scale <= 1 || d <= 0 {
		return d
	}
	w := d / time.Duration(c.Scale)
	if w <= 0 {
		return time.Nanosecond
	}
	return w
}

// Sleep waits the wall-scaled duration, then advances logical time by d.
// A cancelled context does not advance the clock.
func (c *ScaledClock) Sleep(ctx context.Context, d time.Duration) error {
	if c == nil || d <= 0 {
		return nil
	}
	if err := (wallClock{}).Sleep(ctx, c.Wall(d)); err != nil {
		return err
	}
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	return nil
}
