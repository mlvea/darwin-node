package vzcontract

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
)

// Run drives factory through the Day-1 contract. The factory's VMs must
// not leak goroutines or file descriptors after each subtest.
func Run(t *testing.T, factory Factory) {
	t.Helper()
	if factory == nil {
		t.Fatal("nil factory")
	}
	t.Run("order", func(t *testing.T) { testOrder(t, factory) })
	t.Run("reentry", func(t *testing.T) { testReentry(t, factory) })
	t.Run("connect", func(t *testing.T) { testConnect(t, factory) })
	t.Run("vsock", func(t *testing.T) { testVsock(t, factory) })
	t.Run("quota", func(t *testing.T) { testQuota(t, factory) })
	t.Run("save", func(t *testing.T) { testSave(t, factory) })
	t.Run("dgram", func(t *testing.T) { testDgram(t, factory) })
	t.Run("time", func(t *testing.T) { testTime(t, factory) })
}

func prepare(t *testing.T) {
	t.Helper()
	leakcheck.Check(t)
	checkFDs(t)
}

func specNamed(name string) Spec {
	return Spec{
		Name:          name,
		Namespace:     "ns",
		UID:           "uid-" + name,
		HardwareModel: "hw-model",
		MAC:           "02:00:00:00:00:11",
		CPU:           2,
		MemoryBytes:   1 << 30,
	}
}

func mustCreate(t *testing.T, rt Runtime, spec Spec) VM {
	t.Helper()
	vm, err := rt.Create(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if vm.State() != "Stopped" {
		t.Fatalf("create state %s", vm.State())
	}
	if vm.QuotaHeld() {
		t.Fatal("create held a VM slot")
	}
	return vm
}

func mustStart(t *testing.T, vm VM) {
	t.Helper()
	if err := vm.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if vm.State() != "Running" || !vm.QuotaHeld() {
		t.Fatalf("after start state %s held %v", vm.State(), vm.QuotaHeld())
	}
}

type coder interface {
	Domain() string
	Code() int
	FrameworkName() string
}

func requireCode(t *testing.T, err error, code int, name string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s code %d, got nil", name, code)
	}
	var c coder
	if !errors.As(err, &c) || c == nil {
		t.Fatalf("error %v has no VZ domain", err)
	}
	if c.Domain() != DomainVZ || c.Code() != code || c.FrameworkName() != name {
		t.Fatalf("error domain %s code %d name %s, want %s %d %s (%v)",
			c.Domain(), c.Code(), c.FrameworkName(), DomainVZ, code, name, err)
	}
}

// hold runs op on another goroutine and checks the machine while it is
// blocked on an armed barrier. Release is deferred so a failing check
// cannot leave the actor parked.
func hold(t *testing.T, rt Runtime, name string, op func() error, check func()) {
	t.Helper()
	rt.ArmBarrier(name)
	errc := make(chan error, 1)
	go func() { errc <- op() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := rt.WaitBarrier(ctx, name); err != nil {
		rt.ReleaseBarrier(name)
		select {
		case opErr := <-errc:
			t.Fatalf("barrier %s: %v (op %v)", name, err, opErr)
		case <-time.After(2 * time.Second):
			t.Fatalf("barrier %s: %v (op stuck)", name, err)
		}
	}
	check()
	rt.ReleaseBarrier(name)
	if err := <-errc; err != nil {
		t.Fatalf("after %s: %v", name, err)
	}
}

func wantEvents(t *testing.T, got, want []Event) {
	t.Helper()
	if sameEvents(got, want) {
		return
	}
	t.Fatalf("events\n got %s\nwant %s", eventsJSON(got), eventsJSON(want))
}

func bg() context.Context { return context.Background() }

func fmtSeed(seed int64, err error) string {
	return fmt.Sprintf("seed %d: %v", seed, err)
}
