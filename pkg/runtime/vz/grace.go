package vz

import (
	"context"
	"time"
)

// gracePollInterval is how often Stop looks at the VM while a graceful
// shutdown is in progress. The wait returns on the first poll that sees
// the VM stopped, so a pod delete does not sit out terminationGracePeriodSeconds
// after the guest has already halted.
const gracePollInterval = 50 * time.Millisecond

// vmState is the part of the Virtualization.framework state machine that
// Stop needs. It is not the framework type, so the wait can be tested on Linux.
type vmState int

const (
	vmStateRunning vmState = iota
	vmStateStopping
	vmStateStopped
)

// graceClock is the time source for the graceful wait. Tests advance it
// without sleeping. A nil clock is not used; callers pass systemGraceClock.
type graceClock interface {
	Now() time.Time
	NewTimer(d time.Duration) graceTimer
}

// graceTimer is one wait. Stop releases the timer when the VM state is
// known or the caller is cancelled.
type graceTimer interface {
	C() <-chan time.Time
	Stop() bool
}

type systemGraceClock struct{}

func (systemGraceClock) Now() time.Time { return time.Now() }

func (systemGraceClock) NewTimer(d time.Duration) graceTimer {
	if d < 0 {
		d = 0
	}
	return systemGraceTimer{t: time.NewTimer(d)}
}

type systemGraceTimer struct{ t *time.Timer }

func (t systemGraceTimer) C() <-chan time.Time { return t.t.C }

func (t systemGraceTimer) Stop() bool { return t.t.Stop() }

// waitUntilStopped polls state until the VM is stopped, ctx is cancelled,
// or graceful has elapsed. It returns true only when state reports stopped.
// A non-positive graceful does not poll: the caller force-stops.
// The first observation is immediate, so a VM that is already stopped does
// not consume the grace period.
func waitUntilStopped(ctx context.Context, graceful time.Duration, clock graceClock, state func() vmState) bool {
	if state == nil {
		return false
	}
	if state() == vmStateStopped {
		return true
	}
	if graceful <= 0 || clock == nil {
		return false
	}
	deadline := clock.Now().Add(graceful)
	for {
		if state() == vmStateStopped {
			return true
		}
		if ctx.Err() != nil {
			return state() == vmStateStopped
		}
		remain := deadline.Sub(clock.Now())
		if remain <= 0 {
			return state() == vmStateStopped
		}
		step := gracePollInterval
		if remain < step {
			step = remain
		}
		timer := clock.NewTimer(step)
		select {
		case <-ctx.Done():
			timer.Stop()
			return state() == vmStateStopped
		case <-timer.C():
		}
	}
}
