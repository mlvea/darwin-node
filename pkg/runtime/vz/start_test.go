package vz

import (
	"errors"
	"io"
	"testing"
	"time"
)

func TestFinishStartClosesResourcesWhenStartFails(t *testing.T) {
	ln := &closeSpy{err: errors.New("listener")}
	console := &closeSpy{err: errors.New("console")}
	startErr := errors.New("vm start")
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)

	started, err := finishStart(startErr, ln, console, now)
	if !errors.Is(err, startErr) {
		t.Fatalf("err %v, want the start error", err)
	}
	if started != nil {
		t.Fatalf("started %v, a failed start is not a running VM", started)
	}
	if ln.n != 1 || console.n != 1 {
		t.Fatalf("listener closes=%d console closes=%d, resources stayed open", ln.n, console.n)
	}
}

func TestFinishStartKeepsResourcesWhenStartSucceeds(t *testing.T) {
	ln := &closeSpy{}
	console := &closeSpy{}
	now := time.Date(2026, 11, 19, 1, 2, 3, 0, time.UTC)

	started, err := finishStart(nil, ln, console, now)
	if err != nil {
		t.Fatal(err)
	}
	if started == nil || !started.Equal(now) {
		t.Fatalf("started %v, want %v", started, now)
	}
	if ln.n != 0 || console.n != 0 {
		t.Fatalf("listener closes=%d console closes=%d, a running VM must keep its listener and console", ln.n, console.n)
	}
}

func TestFinishStartAllowsNilClosers(t *testing.T) {
	var ln io.Closer
	var console io.Closer
	startErr := errors.New("vm start")
	started, err := finishStart(startErr, ln, console, time.Now())
	if !errors.Is(err, startErr) || started != nil {
		t.Fatalf("started=%v err=%v", started, err)
	}
}
