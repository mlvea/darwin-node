package vzcontract

import (
	"fmt"
	"strings"
	"testing"
)

func testOrder(t *testing.T, factory Factory) {
	t.Run("kvo_before_completion", func(t *testing.T) {
		prepare(t)
		rt := factory(t, Config{Order: OrderKVOBeforeCompletion})
		vm := mustCreate(t, rt, specNamed("kvo"))
		mustStart(t, vm)
		var mid []Event
		var midState string
		hold(t, rt, BarrierBeforeStopCompletion, func() error { return vm.Stop(bg()) }, func() {
			mid = append([]Event(nil), vm.Events()...)
			midState = vm.State()
		})
		// Completion has not run. KVO Stopped already has.
		if midState != "Stopped" {
			t.Fatalf("state at completion barrier %s", midState)
		}
		wantMid := append(startEvents(), evState("Stopping"), evGuest(), evState("Stopped"))
		wantEvents(t, mid, wantMid)
		wantEvents(t, vm.Events(), append(startEvents(), stopEvents(false)...))
		if vm.QuotaHeld() || rt.Running() != 0 {
			t.Fatalf("stopped held %v running %d", vm.QuotaHeld(), rt.Running())
		}
	})

	t.Run("completion_before_kvo", func(t *testing.T) {
		prepare(t)
		rt := factory(t, Config{Order: OrderCompletionBeforeKVO})
		vm := mustCreate(t, rt, specNamed("comp"))
		mustStart(t, vm)
		var mid []Event
		var midState string
		hold(t, rt, BarrierBeforeKVOStopped, func() error { return vm.Stop(bg()) }, func() {
			mid = append([]Event(nil), vm.Events()...)
			midState = vm.State()
		})
		// Completion already ran. KVO Stopped has not.
		if midState != "Stopping" {
			t.Fatalf("state at kvo barrier %s", midState)
		}
		wantMid := append(startEvents(), evState("Stopping"), evGuest(), evOK())
		wantEvents(t, mid, wantMid)
		wantEvents(t, vm.Events(), append(startEvents(), stopEvents(true)...))
	})

	t.Run("request_stop", func(t *testing.T) {
		prepare(t)
		rt := factory(t, Config{Order: OrderCompletionBeforeKVO})
		vm := mustCreate(t, rt, specNamed("req"))
		mustStart(t, vm)
		if err := vm.RequestStop(bg()); err != nil {
			t.Fatal(err)
		}
		wantEvents(t, vm.Events(), append(startEvents(), stopEvents(true)...))
		if vm.State() != "Stopped" {
			t.Fatalf("state %s", vm.State())
		}
	})

	t.Run("jitter", func(t *testing.T) {
		prepare(t)
		const seed int64 = 42
		a := runStop(t, factory, seed)
		b := runStop(t, factory, seed)
		if !sameEvents(a, b) {
			t.Fatalf("seed %d diverged\n a %s\n b %s", seed, eventsJSON(a), eventsJSON(b))
		}
		if !legalStop(a) {
			t.Fatalf("seed %d illegal %s", seed, eventsJSON(a))
		}
		seenCompletionFirst := false
		seenKVOFirst := false
		for s := int64(1); s <= 32; s++ {
			ev := runStop(t, factory, s)
			if !legalStop(ev) {
				t.Fatalf("seed %d illegal %s", s, eventsJSON(ev))
			}
			if completionBeforeStopped(ev) {
				seenCompletionFirst = true
			} else {
				seenKVOFirst = true
			}
		}
		if !seenCompletionFirst || !seenKVOFirst {
			t.Fatal("jitter did not produce both stop orderings in seeds 1-32")
		}
	})
}

func runStop(t *testing.T, factory Factory, seed int64) []Event {
	t.Helper()
	rt := factory(t, Config{Order: OrderJitter, OrderSeed: seed})
	if rt.OrderSeed() != seed {
		t.Fatalf("order seed %d, runtime has %d", seed, rt.OrderSeed())
	}
	vm := mustCreate(t, rt, specNamed(fmt.Sprintf("j%d", seed)))
	mustStart(t, vm)
	if err := vm.Stop(bg()); err != nil {
		t.Fatal(fmtSeed(seed, err))
	}
	return vm.Events()
}

func legalStop(ev []Event) bool {
	prefix := append(startEvents(), evState("Stopping"))
	if len(ev) != len(prefix)+3 || !sameEvents(ev[:len(prefix)], prefix) {
		return false
	}
	got := map[string]int{}
	for _, e := range ev[len(prefix):] {
		switch {
		case e.Kind == "delegate_guest_did_stop" && e.Err == nil:
			got["guest"]++
		case e.Kind == "state_change" && e.State == "Stopped" && e.Err == nil:
			got["stopped"]++
		case e.Kind == "completion_handler" && e.Err == nil:
			got["done"]++
		default:
			return false
		}
	}
	return got["guest"] == 1 && got["stopped"] == 1 && got["done"] == 1
}

func completionBeforeStopped(ev []Event) bool {
	comp, stopped := -1, -1
	for i, e := range ev {
		if e.Kind == "completion_handler" && e.State == "" {
			comp = i
		}
		if e.Kind == "state_change" && e.State == "Stopped" {
			stopped = i
		}
	}
	return comp >= 0 && stopped >= 0 && comp < stopped
}

func testReentry(t *testing.T, factory Factory) {
	prepare(t)
	rt := factory(t, Config{})
	vm := mustCreate(t, rt, specNamed("re"))
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		vm.OnQueue(func() {
			_ = vm.Stop(bg())
		})
	}()
	msg := fmt.Sprint(recovered)
	if recovered == nil || !strings.Contains(msg, "synchronous re-entry") {
		t.Fatalf("queue re-entry: %v", recovered)
	}
	if vm.State() != "Stopped" || vm.QuotaHeld() {
		t.Fatalf("after re-entry state %s held %v", vm.State(), vm.QuotaHeld())
	}
}
