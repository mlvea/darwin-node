package engine

import (
	"testing"
	"time"
)

func TestVirtualClockAdvanceFiresTimer(t *testing.T) {
	c := NewVirtualClock(time.Unix(0, 0).UTC())
	timer := c.NewTimer(time.Minute)
	select {
	case <-timer.C():
		t.Fatal("timer fired before Advance")
	default:
	}
	c.Advance(time.Minute - time.Nanosecond)
	select {
	case <-timer.C():
		t.Fatal("timer fired a nanosecond early")
	default:
	}
	c.Advance(time.Nanosecond)
	select {
	case got := <-timer.C():
		if !got.Equal(c.Now()) {
			t.Fatalf("fired at %s, clock %s", got, c.Now())
		}
	default:
		t.Fatal("timer did not fire")
	}
	if !c.Now().Equal(time.Unix(0, 0).UTC().Add(time.Minute)) {
		t.Fatalf("now %s", c.Now())
	}
}

func TestAgentRedialPaceRestores(t *testing.T) {
	prevB, prevM, prevG := agentRedialBackoff, agentRedialMax, agentRedialBudget
	restore := SetAgentRedialPaceForTest(time.Millisecond, 5*time.Millisecond, 30*time.Millisecond)
	if agentRedialBackoff != time.Millisecond || agentRedialMax != 5*time.Millisecond || agentRedialBudget != 30*time.Millisecond {
		t.Fatalf("pace %s %s %s", agentRedialBackoff, agentRedialMax, agentRedialBudget)
	}
	restore()
	if agentRedialBackoff != prevB || agentRedialMax != prevM || agentRedialBudget != prevG {
		t.Fatalf("restored %s %s %s, want %s %s %s", agentRedialBackoff, agentRedialMax, agentRedialBudget, prevB, prevM, prevG)
	}
}
