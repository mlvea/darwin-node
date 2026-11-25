package vzcontract

import (
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
)

// testTime is the 35s stall against the 30s idle timeout. A manual clock
// advances logical time inside Sleep and does not wait on the wall clock,
// so the case finishes in milliseconds. testing/synctest cannot drive it:
// the guest blocks in a socket read, which is not a durable bubble block.
func testTime(t *testing.T, factory Factory) {
	idle := guest.DefaultIdleTimeout
	t.Run("stall_past_idle", func(t *testing.T) {
		prepare(t)
		stall := idle + 5*time.Second
		rt := factory(t, Config{ManualClock: true, Stall: stall})
		vm := mustCreate(t, rt, specNamed("stall"))
		mustStart(t, vm)
		before := rt.Now()
		wall := time.Now()
		err := vm.DialAgent(bg())
		if err == nil {
			t.Fatal("stall past idle succeeded")
		}
		logical := rt.Now().Sub(before)
		if logical < idle || logical >= stall {
			t.Fatalf("logical %s, want >= %s and < %s", logical, idle, stall)
		}
		if time.Since(wall) > time.Second {
			t.Fatalf("wall %s", time.Since(wall))
		}
		if vm.State() != "Running" || !vm.QuotaHeld() || rt.Running() != 1 {
			t.Fatalf("state %s held %v running %d", vm.State(), vm.QuotaHeld(), rt.Running())
		}
	})

	t.Run("stall_inside_idle", func(t *testing.T) {
		prepare(t)
		const stall = 10 * time.Second
		rt := factory(t, Config{ManualClock: true, Stall: stall})
		vm := mustCreate(t, rt, specNamed("short"))
		mustStart(t, vm)
		before := rt.Now()
		wall := time.Now()
		if err := vm.DialAgent(bg()); err != nil {
			t.Fatal(err)
		}
		logical := rt.Now().Sub(before)
		if logical < stall || logical >= guest.DefaultIdleTimeout {
			t.Fatalf("logical %s", logical)
		}
		if time.Since(wall) > 2*time.Second {
			t.Fatalf("wall %s", time.Since(wall))
		}
		if vm.State() != "Running" {
			t.Fatalf("state %s", vm.State())
		}
	})
}
