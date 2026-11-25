package vzcontract

import (
	"errors"
	"syscall"
	"testing"
	"time"
)

func testConnect(t *testing.T, factory Factory) {
	t.Run("refused_until_register", func(t *testing.T) {
		prepare(t)
		rt := factory(t, Config{HoldAgent: true})
		vm := mustCreate(t, rt, specNamed("conn"))
		mustStart(t, vm)
		start := time.Now()
		_, err := vm.Connect(bg())
		if time.Since(start) > 500*time.Millisecond {
			t.Fatalf("connect blocked for %s", time.Since(start))
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("connect: %v", err)
		}
		if vm.State() != "Running" {
			t.Fatalf("state %s", vm.State())
		}
		wantEvents(t, suffix(vm.Events(), 1), []Event{evErrno("ECONNREFUSED")})
		if err := vm.DialAgent(bg()); !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("dial: %v", err)
		}
		if err := vm.RegisterAgent(bg()); err != nil {
			t.Fatal(err)
		}
		c, err := vm.Connect(bg())
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
	})

	t.Run("delay_50ms", func(t *testing.T) {
		prepare(t)
		rt := factory(t, Config{HoldAgent: true, ConnectDelay: 50 * time.Millisecond})
		vm := mustCreate(t, rt, specNamed("d50"))
		mustStart(t, vm)
		start := time.Now()
		_, err := vm.Connect(bg())
		elapsed := time.Since(start)
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("connect: %v", err)
		}
		if elapsed < 50*time.Millisecond || elapsed > 2*time.Second {
			t.Fatalf("delay %s", elapsed)
		}
	})

	t.Run("delay_2s_manual", func(t *testing.T) {
		prepare(t)
		rt := factory(t, Config{HoldAgent: true, ConnectDelay: 2 * time.Second, ManualClock: true})
		vm := mustCreate(t, rt, specNamed("d2"))
		mustStart(t, vm)
		before := rt.Now()
		wall := time.Now()
		_, err := vm.Connect(bg())
		if !errors.Is(err, syscall.ECONNREFUSED) {
			t.Fatalf("connect: %v", err)
		}
		if time.Since(wall) > time.Second {
			t.Fatalf("wall %s", time.Since(wall))
		}
		if rt.Now().Sub(before) < 2*time.Second {
			t.Fatalf("logical %s", rt.Now().Sub(before))
		}
		if err := vm.RegisterAgent(bg()); err != nil {
			t.Fatal(err)
		}
		mid := rt.Now()
		c, err := vm.Connect(bg())
		if err != nil {
			t.Fatal(err)
		}
		_ = c.Close()
		if rt.Now().Sub(mid) < 2*time.Second {
			t.Fatalf("second delay %s", rt.Now().Sub(mid))
		}
		if time.Since(wall) > 2*time.Second {
			t.Fatalf("wall for both delays %s", time.Since(wall))
		}
	})
}
