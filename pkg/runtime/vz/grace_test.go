package vz

import (
	"context"
	"testing"
	"time"
)

// scriptClock advances as soon as a timer is created, so a wait that
// sleeps the whole grace period shows up as one large step instead of
// a series of polls.
type scriptClock struct {
	now   time.Time
	waits []time.Duration
}

func (c *scriptClock) Now() time.Time { return c.now }

func (c *scriptClock) NewTimer(d time.Duration) graceTimer {
	if d < 0 {
		d = 0
	}
	c.waits = append(c.waits, d)
	c.now = c.now.Add(d)
	ch := make(chan time.Time, 1)
	ch <- c.now
	return scriptTimer{c: ch}
}

type scriptTimer struct{ c chan time.Time }

func (t scriptTimer) C() <-chan time.Time { return t.c }

func (t scriptTimer) Stop() bool { return true }

func TestWaitUntilStoppedReturnsWhenVMStops(t *testing.T) {
	clock := &scriptClock{now: time.Unix(0, 0)}
	// running, stopping, then stopped. A full-grace sleep would record
	// one 30s wait and might never observe the later states.
	seq := []vmState{vmStateRunning, vmStateStopping, vmStateStopped}
	n := 0
	stopped := waitUntilStopped(context.Background(), 30*time.Second, clock, func() vmState {
		s := seq[n]
		if n < len(seq)-1 {
			n++
		}
		return s
	})
	if !stopped {
		t.Fatal("VM reached stopped and the wait did not return")
	}
	if len(clock.waits) == 0 {
		t.Fatal("did not poll")
	}
	var sum time.Duration
	for _, w := range clock.waits {
		if w > gracePollInterval {
			t.Fatalf("wait %s is longer than the poll interval; Stop is sleeping the grace period", w)
		}
		sum += w
	}
	if sum >= 30*time.Second {
		t.Fatalf("waited %s of a 30s grace after the VM had stopped", sum)
	}
}

func TestWaitUntilStoppedAlreadyStoppedDoesNotWait(t *testing.T) {
	clock := &scriptClock{now: time.Unix(0, 0)}
	calls := 0
	stopped := waitUntilStopped(context.Background(), 30*time.Second, clock, func() vmState {
		calls++
		return vmStateStopped
	})
	if !stopped {
		t.Fatal("already stopped")
	}
	if calls != 1 {
		t.Fatalf("state calls %d", calls)
	}
	if len(clock.waits) != 0 {
		t.Fatalf("waited %v for a VM that was already stopped", clock.waits)
	}
}

func TestWaitUntilStoppedHonorsDeadline(t *testing.T) {
	clock := &scriptClock{now: time.Unix(0, 0)}
	grace := 200 * time.Millisecond
	stopped := waitUntilStopped(context.Background(), grace, clock, func() vmState {
		return vmStateRunning
	})
	if stopped {
		t.Fatal("running VM reported stopped at the deadline")
	}
	var sum time.Duration
	for _, w := range clock.waits {
		if w > gracePollInterval {
			t.Fatalf("step %s", w)
		}
		sum += w
	}
	if sum < grace || sum >= grace+gracePollInterval {
		t.Fatalf("waited %s, want the %s grace", sum, grace)
	}
}

func TestWaitUntilStoppedCancelledContextDoesNotSleep(t *testing.T) {
	clock := &scriptClock{now: time.Unix(0, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopped := waitUntilStopped(ctx, time.Minute, clock, func() vmState { return vmStateRunning })
	if stopped {
		t.Fatal("cancelled wait reported the VM stopped")
	}
	if len(clock.waits) != 0 {
		t.Fatalf("cancelled context still waited %v", clock.waits)
	}
}

func TestWaitUntilStoppedZeroGraceDoesNotPoll(t *testing.T) {
	clock := &scriptClock{now: time.Unix(0, 0)}
	stopped := waitUntilStopped(context.Background(), 0, clock, func() vmState { return vmStateRunning })
	if stopped || len(clock.waits) != 0 {
		t.Fatalf("stopped=%v waits=%v", stopped, clock.waits)
	}
}

func TestSystemGraceClockSeesStoppedImmediately(t *testing.T) {
	if !waitUntilStopped(context.Background(), time.Minute, systemGraceClock{}, func() vmState { return vmStateStopped }) {
		t.Fatal("system clock did not observe a stopped VM")
	}
}
