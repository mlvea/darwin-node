package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/runtime/simvz"
	"github.com/mlvea/darwin-node/pkg/sidecar"

	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
)

func simEngine(t *testing.T, opts simvz.Options) (*Engine, *simvz.Runtime) {
	t.Helper()
	leakcheck.Check(t)
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "simvz"
	cfg.CacheDir = t.TempDir()
	cfg.AllowNATWorkloads = true
	cfg.AgentReadyTimeout = 20 * time.Second
	// The watch's first step is immediate. A long interval keeps later
	// probe ticks from starting another VM during the assertion window.
	cfg.ProbeInterval = time.Hour
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	rt := simvz.New(opts)
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	return e, rt
}

func simFDBefore(t *testing.T) int {
	t.Helper()
	if _, err := simvz.FDCount(); err != nil {
		t.Fatal(err)
	}
	n, err := simvz.FDCount()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func simFDBack(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		n, err := simvz.FDCount()
		if err != nil {
			t.Fatal(err)
		}
		// A couple of descriptors show up under the race detector and
		// stay. Growth past that is a socket or listener that Stop missed.
		if n <= before+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("open fds %d, baseline %d", n, before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func finishSimPod(t *testing.T, e *Engine, name string, before int, leaks goleak.Option) {
	t.Helper()
	defer goleak.VerifyNone(t, leaks)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Delete(ctx, "default", name, 0); err != nil {
		t.Errorf("delete: %v", err)
	}
	if e.Slots().Used() != 0 {
		t.Errorf("slots still held: %d", e.Slots().Used())
	}
	simFDBack(t, before)
}

func waitMessage(t *testing.T, e *Engine, name, substr string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		p, err := e.Get("default", name)
		if err == nil {
			last = p.Status.Message
			if strings.Contains(last, substr) || strings.Contains(p.Status.Reason, substr) {
				return p
			}
			if p.Status.Phase == corev1.PodFailed {
				t.Fatalf("pod failed: %s %s", p.Status.Reason, p.Status.Message)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pod %s message %q never contained %q", name, last, substr)
	return nil
}

func TestSimVZEngineLifecycle(t *testing.T) {
	bin, err := simvz.GuestAgentBinary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	leaks := goleak.IgnoreCurrent()
	before := simFDBefore(t)
	e, rt := simEngine(t, simvz.Options{GuestBinary: bin})
	defer finishSimPod(t, e, "sim", before, leaks)
	if err := e.Create(context.Background(), samplePod("sim", "uid-sim"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "sim", corev1.PodRunning)
	if p.Status.PodIP == "" {
		t.Fatal("running pod has no IP")
	}
	if rt.Running() != 1 {
		t.Fatalf("runtime running %d", rt.Running())
	}
}

func TestSimVZEngineStartFailureRetries(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	before := simFDBefore(t)
	e, _ := simEngine(t, simvz.Options{Faults: simvz.Faults{FailStart: 1}})
	defer finishSimPod(t, e, "boot", before, leaks)
	if err := e.Create(context.Background(), samplePod("boot", "uid-boot"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	// The first start() fails and the engine backs off, then a new machine
	// starts. The pod should still reach Running. The message may already
	// have been cleared by the time we look, so wait on the phase.
	waitPhase(t, e, "default", "boot", corev1.PodRunning)
}

func TestSimVZEngineVsockEOFCleansUp(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	before := simFDBefore(t)
	e, rt := simEngine(t, simvz.Options{Faults: simvz.Faults{VsockEOFAfter: 6}})
	defer finishSimPod(t, e, "eof", before, leaks)
	if err := e.Create(context.Background(), samplePod("eof", "uid-eof"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitMessage(t, e, "eof", "back-off")
	if p.Status.Phase == corev1.PodRunning {
		t.Fatal("mid-frame EOF still marked the pod running")
	}
	if rt.Running() != 1 {
		t.Fatalf("dial keeps the slot, running %d", rt.Running())
	}
}

func TestSimVZEngineStallDropsSession(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	before := simFDBefore(t)
	e, _ := simEngine(t, simvz.Options{
		Clock:  simvz.NewScaledClock(8),
		Faults: simvz.Faults{Stall: 50 * time.Second},
	})
	defer finishSimPod(t, e, "stall", before, leaks)
	if err := e.Create(context.Background(), samplePod("stall", "uid-stall"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitMessage(t, e, "stall", "back-off")
}

func TestSimVZEngineGuestCrashCleansUp(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	before := simFDBefore(t)
	e, rt := simEngine(t, simvz.Options{Faults: simvz.Faults{CrashGuest: true}})
	defer finishSimPod(t, e, "crash", before, leaks)
	if err := e.Create(context.Background(), samplePod("crash", "uid-crash"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitMessage(t, e, "crash", "back-off")
	if rt.Running() != 1 {
		t.Fatalf("crashed guest should leave the VM running, got %d", rt.Running())
	}
}
