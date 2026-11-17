package vz

import (
	"errors"
	"io"
	"testing"
	"time"
)

type closeSpy struct {
	n   int
	err error
}

func (c *closeSpy) Close() error {
	c.n++
	return c.err
}

func TestFinishStopClosesResourcesWhenStopFails(t *testing.T) {
	ln := &closeSpy{err: errors.New("listener")}
	console := &closeSpy{err: errors.New("console")}
	stopErr := errors.New("vm stop")
	now := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)

	finished, err := finishStop(stopErr, ln, console, now)
	if !errors.Is(err, stopErr) {
		t.Fatalf("err %v, want the stop error", err)
	}
	if finished != nil {
		t.Fatalf("finished %v, a failed stop is not a clean shutdown", finished)
	}
	if ln.n != 1 || console.n != 1 {
		t.Fatalf("listener closes=%d console closes=%d, resources stayed open", ln.n, console.n)
	}
}

func TestFinishStopClosesResourcesWhenStopSucceeds(t *testing.T) {
	ln := &closeSpy{}
	console := &closeSpy{}
	now := time.Date(2026, 11, 17, 1, 2, 3, 0, time.UTC)

	finished, err := finishStop(nil, ln, console, now)
	if err != nil {
		t.Fatal(err)
	}
	if finished == nil || !finished.Equal(now) {
		t.Fatalf("finished %v, want %v", finished, now)
	}
	if ln.n != 1 || console.n != 1 {
		t.Fatalf("listener closes=%d console closes=%d", ln.n, console.n)
	}
}

func TestFinishStopAllowsNilClosers(t *testing.T) {
	var ln io.Closer
	var console io.Closer
	now := time.Now()
	finished, err := finishStop(nil, ln, console, now)
	if err != nil || finished == nil || !finished.Equal(now) {
		t.Fatalf("finished=%v err=%v", finished, err)
	}
}
