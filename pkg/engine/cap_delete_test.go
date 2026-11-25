package engine

import (
	"context"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	rtpkg "github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"
)

// holdCreateRuntime blocks inside the first Create until release is closed.
// Later creates wait for that first call to return, then pass through.
// Engine.Create admits a pod before it reaches Runtime.Create, so the test
// can ask for more pods while this first VM is still being created.
type holdCreateRuntime struct {
	*fake.Runtime
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *holdCreateRuntime) Create(ctx context.Context, spec types.VMSpec) (rtpkg.Machine, error) {
	h.once.Do(func() {
		close(h.entered)
		<-h.release
	})
	return h.Runtime.Create(ctx, spec)
}

// deleteWaitingForStart reports whether some goroutine is inside Engine.Delete
// blocked on startDone. That is the moment a delete has either already dropped
// the pod's slot (the bug) or is still holding it until start returns.
func deleteWaitingForStart() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "(*Engine).Delete") && strings.Contains(g, "(*WaitGroup).Wait") {
			return true
		}
	}
	return false
}

// TestDeleteDuringInFlightCreateDoesNotAdmitAThirdVM is the 2-VM cap while a
// pod is deleted during Runtime.Create. The slot has to stay held until start
// can no longer boot a machine. Releasing it early lets two more pods in.
func TestDeleteDuringInFlightCreateDoesNotAdmitAThirdVM(t *testing.T) {
	leakcheck.Check(t)
	release := make(chan struct{})
	var releaseOnce sync.Once
	letCreateReturn := func() { releaseOnce.Do(func() { close(release) }) }

	rt := &holdCreateRuntime{
		Runtime: fake.New(),
		entered: make(chan struct{}),
		release: release,
	}
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")

	delDone := make(chan struct{})
	deleteStarted := false
	t.Cleanup(func() {
		letCreateReturn()
		if deleteStarted {
			select {
			case <-delDone:
			case <-time.After(5 * time.Second):
				t.Errorf("delete did not return after Runtime.Create unblocked")
			}
		}
		_ = e.Delete(context.Background(), "default", "b", 0)
		_ = e.Delete(context.Background(), "default", "c", 0)
		e.Close()
	})

	if err := e.Create(context.Background(), samplePod("a", "uid-a"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Runtime.Create was not reached")
	}

	deleteStarted = true
	go func() {
		defer close(delDone)
		_ = e.Delete(context.Background(), "default", "a", 0)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !deleteWaitingForStart() {
		if time.Now().After(deadline) {
			t.Fatal("Delete did not reach startDone.Wait while Runtime.Create was blocked")
		}
		time.Sleep(time.Millisecond)
	}
	usedAtWait := e.slots.Used()
	if usedAtWait != 1 {
		t.Fatalf("pod a released its slot while Runtime.Create was still running: used=%d", usedAtWait)
	}

	// Both may be accepted: one free slot remains, and the slot is taken
	// inside start rather than in Create. The third VM must not enter
	// Runtime.Create while a and one of these hold the two slots.
	errB := e.Create(context.Background(), samplePod("b", "uid-b"), Credentials{})
	errC := e.Create(context.Background(), samplePod("c", "uid-c"), Credentials{})
	if errB != nil || errC != nil {
		t.Fatalf("admission while one slot is free: b=%v c=%v", errB, errC)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && e.slots.Used() < 2 {
		time.Sleep(time.Millisecond)
	}
	if used := e.slots.Used(); used != 2 {
		t.Fatalf("slots=%d, want both slots held while Runtime.Create has not returned", used)
	}
	if got := rt.Running(); got != 0 {
		t.Fatalf("running=%d before Runtime.Create returned", got)
	}
	letCreateReturn()
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := rt.Running(); got > 2 {
			t.Fatalf("running VMs %d exceed the 2-slot cap", got)
		}
		time.Sleep(time.Millisecond)
	}
}
