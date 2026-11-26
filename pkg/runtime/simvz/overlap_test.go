package simvz

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestStartOverlapsStopAcceptWait is the PR #65 cleanup hang.
//
// Semantics, matching Virtualization.framework canStart: Start is legal
// from Stopped or Error, and a stopped VM can be started again. Start
// while the state is still Stopping returns VZErrorInvalidVirtualMachineState
// and does not wait. Once Stop has published Stopped, Start is legal even
// if that Stop call is still draining the previous boot. Cleanup waits
// only for accept loops that belong to the generation it stopped. It does
// not wait for, or close, a generation Start installs afterwards, and
// Stop returns. Start is not serialized behind Stop returning: canStart
// is already true. The engine's guest watch depends on that window. It
// runs from inside cleanup, sees Stopped, and calls Start before Stop
// returns.
func TestStartOverlapsStopAcceptWait(t *testing.T) {
	before := fdBefore(t)
	rt := newRT(t, Options{})
	m := mustMachine(t, rt, "overlap")
	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if m.State() != "Running" {
		t.Fatalf("state %s", m.State())
	}

	rt.ArmBarrier(BarrierBeforeAcceptWait)
	stopErr := make(chan error, 1)
	go func() { stopErr <- m.Stop(ctx, 0) }()
	waitBarrier(t, rt, BarrierBeforeAcceptWait)
	if m.State() != "Stopped" {
		t.Fatalf("state at accept wait %s, want Stopped", m.State())
	}

	startErr := make(chan error, 1)
	go func() { startErr <- m.Start(ctx) }()
	select {
	case err := <-startErr:
		if err != nil {
			rt.ReleaseBarrier(BarrierBeforeAcceptWait)
			t.Fatalf("start while cleanup drains: %v", err)
		}
	case <-time.After(2 * time.Second):
		rt.ReleaseBarrier(BarrierBeforeAcceptWait)
		t.Fatal("Start did not return while Stop was draining the previous accept loops")
	}
	if m.State() != "Running" {
		rt.ReleaseBarrier(BarrierBeforeAcceptWait)
		t.Fatalf("after overlapping Start, state %s", m.State())
	}
	rt.ReleaseBarrier(BarrierBeforeAcceptWait)

	select {
	case err := <-stopErr:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		// Closing the listener Start installed lets the loop cleanup is
		// waiting on leave Accept, so this failure does not stick the
		// process. The assertion is that Stop should already have returned.
		m.mu.Lock()
		ln := m.ln
		m.mu.Unlock()
		if ln != nil {
			_ = ln.Close()
		}
		select {
		case <-stopErr:
		case <-time.After(2 * time.Second):
		}
		t.Fatal("Stop did not return: cleanup waited on an accept loop registered by Start")
	}

	if m.State() != "Running" {
		t.Fatalf("Stop tore down the generation Start installed: state %s", m.State())
	}
	dialCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cli, err := m.DialAgent(dialCtx)
	if err != nil {
		t.Fatalf("dial after overlapping start: %v", err)
	}
	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	if rt.Running() != 1 {
		t.Fatalf("running %d", rt.Running())
	}
	if err := m.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if m.State() != "Stopped" || rt.Running() != 0 {
		t.Fatalf("after final stop state %s running %d", m.State(), rt.Running())
	}
	fdBack(t, before)
}

// TestStartWhileStoppingIsInvalid is the other half of canStart. Stopping
// is not Stopped. Start fails with VZErrorInvalidVirtualMachineState and
// the in-flight Stop still completes.
func TestStartWhileStoppingIsInvalid(t *testing.T) {
	before := fdBefore(t)
	rt := newRT(t, Options{Order: OrderKVOBeforeCompletion})
	m := mustMachine(t, rt, "stopping")
	ctx := context.Background()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	rt.ArmBarrier(BarrierBeforeKVOStopped)
	stopErr := make(chan error, 1)
	go func() { stopErr <- m.Stop(ctx, 0) }()
	waitBarrier(t, rt, BarrierBeforeKVOStopped)
	if m.State() != "Stopping" {
		rt.ReleaseBarrier(BarrierBeforeKVOStopped)
		t.Fatalf("state %s, want Stopping", m.State())
	}
	err := m.Start(ctx)
	rt.ReleaseBarrier(BarrierBeforeKVOStopped)
	var se *StateError
	if !errors.As(err, &se) || se.Code() != 3 || se.FrameworkName() != "VZErrorInvalidVirtualMachineState" {
		t.Fatalf("start while stopping: %v", err)
	}
	select {
	case err := <-stopErr:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not return after Start was refused")
	}
	if m.State() != "Stopped" || rt.Running() != 0 {
		t.Fatalf("after stop state %s running %d", m.State(), rt.Running())
	}
	fdBack(t, before)
}

func waitBarrier(t *testing.T, rt *Runtime, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := rt.WaitBarrier(ctx, name); err != nil {
		t.Fatalf("barrier %s: %v", name, err)
	}
}
