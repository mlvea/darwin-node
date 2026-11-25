package vzcontract

import (
	"path/filepath"
	"testing"
)

func testQuota(t *testing.T, factory Factory) {
	t.Run("held_until_stopped_or_error", func(t *testing.T) {
		prepare(t)
		rt := factory(t, Config{})
		vm := mustCreate(t, rt, specNamed("q"))
		if rt.Running() != 0 {
			t.Fatalf("running before start %d", rt.Running())
		}
		hold(t, rt, BarrierBeforeKVOStarting, func() error { return vm.Start(bg()) }, func() {
			assertHeld(t, rt, vm, "Starting", 1)
		})
		assertHeld(t, rt, vm, "Running", 1)

		hold(t, rt, BarrierBeforeKVOPausing, func() error { return vm.Pause(bg()) }, func() {
			assertHeld(t, rt, vm, "Pausing", 1)
		})
		assertHeld(t, rt, vm, "Paused", 1)

		path := filepath.Join(t.TempDir(), "q.json")
		hold(t, rt, BarrierBeforeKVOSaving, func() error { return vm.Save(bg(), path) }, func() {
			assertHeld(t, rt, vm, "Saving", 1)
		})
		assertHeld(t, rt, vm, "Paused", 1)

		hold(t, rt, BarrierBeforeKVOResuming, func() error { return vm.Resume(bg()) }, func() {
			assertHeld(t, rt, vm, "Resuming", 1)
		})
		assertHeld(t, rt, vm, "Running", 1)

		hold(t, rt, BarrierBeforeKVOStopping, func() error { return vm.Stop(bg()) }, func() {
			assertHeld(t, rt, vm, "Stopping", 1)
		})
		if vm.State() != "Stopped" || vm.QuotaHeld() || rt.Running() != 0 {
			t.Fatalf("after stop %s held %v running %d", vm.State(), vm.QuotaHeld(), rt.Running())
		}

		hold(t, rt, BarrierBeforeKVORestoring, func() error { return vm.Restore(bg(), path) }, func() {
			assertHeld(t, rt, vm, "Restoring", 1)
		})
		assertHeld(t, rt, vm, "Paused", 1)
	})

	t.Run("third_start", func(t *testing.T) {
		for _, order := range []string{OrderKVOBeforeCompletion, OrderCompletionBeforeKVO} {
			t.Run(order, func(t *testing.T) {
				prepare(t)
				rt := factory(t, Config{Order: order})
				a := mustCreate(t, rt, specNamed("a"))
				b := mustCreate(t, rt, specNamed("b"))
				c := mustCreate(t, rt, specNamed("c"))
				if rt.Running() != 0 {
					t.Fatalf("create took slots: %d", rt.Running())
				}
				mustStart(t, a)
				mustStart(t, b)
				err := c.Start(bg())
				requireCode(t, err, CodeVirtualMachineLimitExceeded, NameLimit)
				if c.State() != "Error" || c.QuotaHeld() || rt.Running() != 2 {
					t.Fatalf("loser %s held %v running %d", c.State(), c.QuotaHeld(), rt.Running())
				}
				want := []Event{evState("Error"), evCode(CodeVirtualMachineLimitExceeded, NameLimit)}
				if order == OrderCompletionBeforeKVO {
					want = []Event{evCode(CodeVirtualMachineLimitExceeded, NameLimit), evState("Error")}
				}
				wantEvents(t, c.Events(), want)
				if err := a.Stop(bg()); err != nil {
					t.Fatal(err)
				}
				if rt.Running() != 1 {
					t.Fatalf("after one stop %d", rt.Running())
				}
				mustStart(t, c)
				if rt.Running() != 2 {
					t.Fatalf("retry running %d", rt.Running())
				}
			})
		}
	})

	t.Run("third_restore", func(t *testing.T) {
		prepare(t)
		rt := factory(t, Config{})
		spec := specNamed("saved")
		saved := mustCreate(t, rt, spec)
		mustStart(t, saved)
		if err := saved.Pause(bg()); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "saved.json")
		if err := saved.Save(bg(), path); err != nil {
			t.Fatal(err)
		}
		if err := saved.Stop(bg()); err != nil {
			t.Fatal(err)
		}
		a := mustCreate(t, rt, specNamed("live-a"))
		b := mustCreate(t, rt, specNamed("live-b"))
		mustStart(t, a)
		mustStart(t, b)
		before := len(saved.Events())
		err := saved.Restore(bg(), path)
		requireCode(t, err, CodeVirtualMachineLimitExceeded, NameLimit)
		if saved.State() != "Stopped" || saved.QuotaHeld() || rt.Running() != 2 {
			t.Fatalf("restore loser %s held %v running %d", saved.State(), saved.QuotaHeld(), rt.Running())
		}
		wantEvents(t, saved.Events()[before:], []Event{evCode(CodeVirtualMachineLimitExceeded, NameLimit)})
	})
}

func assertHeld(t *testing.T, rt Runtime, vm VM, state string, running int) {
	t.Helper()
	if vm.State() != state || !vm.QuotaHeld() || rt.Running() != running {
		t.Fatalf("state %s held %v running %d, want %s held true running %d",
			vm.State(), vm.QuotaHeld(), rt.Running(), state, running)
	}
}
