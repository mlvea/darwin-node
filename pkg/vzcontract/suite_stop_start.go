package vzcontract

import (
	"context"
	"testing"
	"time"
)

// testStopStart locks canStart against an in-flight Stop.
//
// Start while the state is still Stopping returns
// VZErrorInvalidVirtualMachineState and does not wait. Once Stopped is
// published, Start is legal even if that Stop call has not returned: a
// stopped VM can be started again. Cleanup waits only for the generation
// it stopped, so the new VM stays Running, accepts a dial, and Stop
// returns.
//
// before_accept_wait is a SimVZ hook, not an Apple KVO point. It parks
// the Stop caller after the stopped generation's listener is closed and
// before that generation's accept-loop wait. Running the same overlap on
// Virtualization.framework is hardware-pending.
func testStopStart(t *testing.T, factory Factory) {
	t.Run("while_stopping", func(t *testing.T) { testStartWhileStopping(t, factory) })
	t.Run("during_accept_drain", func(t *testing.T) { testStartDuringAcceptDrain(t, factory) })
}

func testStartWhileStopping(t *testing.T, factory Factory) {
	prepare(t)
	rt := factory(t, Config{Order: OrderKVOBeforeCompletion})
	vm := mustCreate(t, rt, specNamed("stopping"))
	mustStart(t, vm)
	hold(t, rt, BarrierBeforeKVOStopped, func() error { return vm.Stop(bg()) }, func() {
		if vm.State() != "Stopping" {
			t.Fatalf("state %s, want Stopping", vm.State())
		}
		err := vm.Start(bg())
		requireCode(t, err, CodeInvalidVirtualMachineState, NameInvalidState)
	})
	if vm.State() != "Stopped" || vm.QuotaHeld() || rt.Running() != 0 {
		t.Fatalf("after stop state %s held %v running %d", vm.State(), vm.QuotaHeld(), rt.Running())
	}
}

func testStartDuringAcceptDrain(t *testing.T, factory Factory) {
	prepare(t)
	rt := factory(t, Config{Order: OrderKVOBeforeCompletion})
	vm := mustCreate(t, rt, specNamed("drain"))
	mustStart(t, vm)
	rt.ArmBarrier(BarrierBeforeAcceptWait)
	stopErr := make(chan error, 1)
	go func() { stopErr <- vm.Stop(bg()) }()
	ctx, cancel := context.WithTimeout(bg(), 3*time.Second)
	defer cancel()
	if err := rt.WaitBarrier(ctx, BarrierBeforeAcceptWait); err != nil {
		rt.ReleaseBarrier(BarrierBeforeAcceptWait)
		t.Fatalf("barrier: %v", err)
	}
	if vm.State() != "Stopped" {
		rt.ReleaseBarrier(BarrierBeforeAcceptWait)
		t.Fatalf("state at accept wait %s, want Stopped", vm.State())
	}
	if err := vm.Start(bg()); err != nil {
		rt.ReleaseBarrier(BarrierBeforeAcceptWait)
		t.Fatalf("start during accept drain: %v", err)
	}
	if vm.State() != "Running" || !vm.QuotaHeld() {
		rt.ReleaseBarrier(BarrierBeforeAcceptWait)
		t.Fatalf("after start state %s held %v", vm.State(), vm.QuotaHeld())
	}
	if err := vm.DialAgent(bg()); err != nil {
		rt.ReleaseBarrier(BarrierBeforeAcceptWait)
		t.Fatalf("dial during accept drain: %v", err)
	}
	rt.ReleaseBarrier(BarrierBeforeAcceptWait)
	select {
	case err := <-stopErr:
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return: cleanup waited on an accept loop registered by Start")
	}
	if vm.State() != "Running" || !vm.QuotaHeld() || rt.Running() != 1 {
		t.Fatalf("after the overlapped stop state %s held %v running %d", vm.State(), vm.QuotaHeld(), rt.Running())
	}
	if err := vm.DialAgent(bg()); err != nil {
		t.Fatalf("dial after stop returned: %v", err)
	}
	if err := vm.Stop(bg()); err != nil {
		t.Fatal(err)
	}
	if vm.State() != "Stopped" || vm.QuotaHeld() || rt.Running() != 0 {
		t.Fatalf("final state %s held %v running %d", vm.State(), vm.QuotaHeld(), rt.Running())
	}
}
