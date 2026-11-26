package simvz

import (
	"context"
	"errors"
	"testing"
)

func TestFailNextStartsThenCrashReleases(t *testing.T) {
	before := fdBefore(t)
	rt := newRT(t, Options{Clock: NewManualClock()})
	if err := rt.CrashGuest("default", "missing"); !errors.Is(err, ErrNoRunningGuest) {
		t.Fatalf("crash with no VM: %v", err)
	}
	rt.FailNextStarts(1)
	m := mustMachine(t, rt, "budget")
	ctx := context.Background()
	err := m.Start(ctx)
	if !errors.Is(err, errStartFailed) {
		t.Fatalf("first start: %v", err)
	}
	if rt.Running() != 0 {
		t.Fatalf("failed start held a slot: %d", rt.Running())
	}
	if dirs := rt.LiveCloneDirs(); len(dirs) != 0 {
		t.Fatalf("failed start left clone dirs %v", dirs)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatalf("second start: %v", err)
	}
	if rt.Running() != 1 || m.State() != "Running" {
		t.Fatalf("state %s running %d", m.State(), rt.Running())
	}
	if err := rt.CrashGuest("default", "budget"); err != nil {
		t.Fatal(err)
	}
	if rt.Running() != 1 || m.State() != "Running" {
		t.Fatalf("crash changed state %s running %d", m.State(), rt.Running())
	}
	if _, err := m.DialAgent(ctx); err == nil {
		t.Fatal("dial succeeded after the guest crashed")
	}
	if dirs := rt.LiveCloneDirs(); len(dirs) != 1 {
		t.Fatalf("clone dirs %v, want the running VM", dirs)
	}
	if err := m.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if rt.Running() != 0 {
		t.Fatalf("running %d after stop", rt.Running())
	}
	if dirs := rt.LiveCloneDirs(); len(dirs) != 0 {
		t.Fatalf("stop left clone dirs %v", dirs)
	}
	if err := rt.CrashGuest("default", "budget"); !errors.Is(err, ErrNoRunningGuest) {
		t.Fatalf("crash after stop: %v", err)
	}
	fdBack(t, before)
}
