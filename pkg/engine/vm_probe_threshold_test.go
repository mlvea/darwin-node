package engine

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"

	corev1 "k8s.io/api/core/v1"
)

// vmProbePod is a single-container VM pod whose probes the test counts by argv.
func vmProbePod(name, uid string) *corev1.Pod {
	p := samplePod(name, uid)
	p.Spec.RestartPolicy = corev1.RestartPolicyAlways
	return p
}

func (c *manualClock) armed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.waiters {
		if !t.stopped {
			return true
		}
	}
	return false
}

// waitSamples blocks until the probe has returned at least want times and the
// watch has gone back to sleep on the fake clock. The count moves inside the
// probe, before the streak is published, so the timer is the publish barrier.
func waitSamples(t *testing.T, clk *manualClock, n *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n.Load() >= want && clk.armed() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("probe settled at %d samples, want %d", n.Load(), want)
}

func clockedVM(t *testing.T, exec func(context.Context, guest.ExecReq, io.Reader, io.Writer, io.Writer) (int, error)) (*Engine, *fake.Runtime, *manualClock) {
	t.Helper()
	clk := newManualClock(time.Now())
	rt := fake.New()
	rt.ExecFn = exec
	e := sidecarEngine(t, rt, nil)
	e.clock = clk
	e.cfg.ProbeInterval = time.Hour
	return e, rt, clk
}

// Readiness must stay false until successThreshold consecutive successes.
// One passing sample currently marks the container Ready.
func TestVMReadinessSuccessThreshold(t *testing.T) {
	var n atomic.Int32
	e, _, clk := clockedVM(t, func(_ context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "ready" {
			n.Add(1)
		}
		return 0, nil
	})
	pod := vmProbePod("rd", "uid-vm-ready-ok")
	pod.Spec.Containers[0].ReadinessProbe = execProbe("ready", 5, 1, 8, 3)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSamples(t, clk, &n, 1)
	p, err := e.Get("default", "rd")
	if err != nil {
		t.Fatal(err)
	}
	main := containerStatus(p, "macos")
	if main != nil && main.Ready && n.Load() < 3 {
		t.Fatalf("ready after %d successes; successThreshold is 3", n.Load())
	}
	if main == nil || main.Ready || podReady(p) {
		t.Fatalf("readiness flipped before successThreshold: samples=%d %+v", n.Load(), main)
	}
	clk.Advance(5 * time.Second)
	waitSamples(t, clk, &n, 2)
	p, _ = e.Get("default", "rd")
	main = containerStatus(p, "macos")
	if main != nil && (main.Ready || podReady(p)) {
		t.Fatalf("ready after %d successes; successThreshold is 3", n.Load())
	}
	clk.Advance(5 * time.Second)
	waitSamples(t, clk, &n, 3)
	p, _ = e.Get("default", "rd")
	main = containerStatus(p, "macos")
	if main == nil || !main.Ready || !podReady(p) || !statusStarted(main) {
		t.Fatalf("readiness did not pass after successThreshold: samples=%d %+v podReady=%v", n.Load(), main, podReady(p))
	}
	if e.slots.Used() != 1 {
		t.Fatalf("slots=%d", e.slots.Used())
	}
}

// A container that has become Ready stays Ready until failureThreshold
// consecutive failures. One failing sample currently clears Ready.
func TestVMReadinessFailureThreshold(t *testing.T) {
	var n atomic.Int32
	var fail atomic.Bool
	e, _, clk := clockedVM(t, func(_ context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "ready" {
			n.Add(1)
			if fail.Load() {
				return 1, nil
			}
		}
		return 0, nil
	})
	pod := vmProbePod("rd", "uid-vm-ready-fail")
	pod.Spec.Containers[0].ReadinessProbe = execProbe("ready", 5, 1, 3, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSamples(t, clk, &n, 1)
	p, err := e.Get("default", "rd")
	if err != nil {
		t.Fatal(err)
	}
	main := containerStatus(p, "macos")
	if main == nil || !main.Ready || !podReady(p) {
		t.Fatalf("first success did not mark Ready: samples=%d %+v", n.Load(), main)
	}
	fail.Store(true)
	passed := n.Load()
	for i := int32(1); i < 3; i++ {
		clk.Advance(5 * time.Second)
		waitSamples(t, clk, &n, passed+i)
		p, _ = e.Get("default", "rd")
		main = containerStatus(p, "macos")
		fails := n.Load() - passed
		if main == nil || !main.Ready || !podReady(p) {
			t.Fatalf("readiness flipped before failureThreshold: fails=%d %+v podReady=%v", fails, main, podReady(p))
		}
	}
	clk.Advance(5 * time.Second)
	waitSamples(t, clk, &n, passed+3)
	p, _ = e.Get("default", "rd")
	main = containerStatus(p, "macos")
	if main == nil || main.Ready || podReady(p) {
		t.Fatalf("readiness stayed true after failureThreshold: fails=%d %+v", n.Load()-passed, main)
	}
	c, ok := cond(p, corev1.PodReady)
	if !ok || c.Reason != "ReadinessProbe" {
		t.Fatalf("PodReady %+v", c)
	}
	if e.slots.Used() != 1 {
		t.Fatalf("slots=%d", e.slots.Used())
	}
}

// Startup stays not-started until successThreshold consecutive successes.
// One passing sample currently sets Started.
func TestVMStartupSuccessThreshold(t *testing.T) {
	var boots, lives atomic.Int32
	e, rt, clk := clockedVM(t, func(_ context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
		if len(req.Argv) == 0 {
			return 0, nil
		}
		switch req.Argv[0] {
		case "boot":
			boots.Add(1)
			return 0, nil
		case "live":
			lives.Add(1)
			return 0, nil
		default:
			return 0, nil
		}
	})
	pod := vmProbePod("boot", "uid-vm-boot-ok")
	pod.Spec.Containers[0].StartupProbe = execProbe("boot", 5, 1, 8, 3)
	pod.Spec.Containers[0].LivenessProbe = execProbe("live", 5, 1, 1, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSamples(t, clk, &boots, 1)
	p, err := e.Get("default", "boot")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase == corev1.PodFailed {
		t.Fatalf("startup probe failed the pod: %s", p.Status.Message)
	}
	main := containerStatus(p, "macos")
	if main != nil && statusStarted(main) && boots.Load() < 3 {
		t.Fatalf("Started after %d successes; successThreshold is 3", boots.Load())
	}
	if main == nil || statusStarted(main) || main.Ready || lives.Load() != 0 {
		t.Fatalf("startup passed early: boots=%d lives=%d %+v", boots.Load(), lives.Load(), main)
	}
	clk.Advance(5 * time.Second)
	waitSamples(t, clk, &boots, 2)
	p, _ = e.Get("default", "boot")
	main = containerStatus(p, "macos")
	if main != nil && (statusStarted(main) || lives.Load() != 0) {
		t.Fatalf("Started after %d successes; successThreshold is 3 (lives=%d)", boots.Load(), lives.Load())
	}
	clk.Advance(5 * time.Second)
	waitSamples(t, clk, &boots, 3)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p, err = e.Get("default", "boot")
		if err == nil {
			main = containerStatus(p, "macos")
			if main != nil && statusStarted(main) && main.Ready && podReady(p) && lives.Load() > 0 {
				if rt.Starts() != 1 || e.slots.Used() != 1 {
					t.Fatalf("starts=%d slots=%d", rt.Starts(), e.slots.Used())
				}
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	p, _ = e.Get("default", "boot")
	t.Fatalf("startup did not pass after successThreshold: boots=%d lives=%d %+v", boots.Load(), lives.Load(), containerStatus(p, "macos"))
}

// Liveness already counts failureThreshold. One failure must not restart the
// VM when the threshold is 3, and the third failure must.
func TestVMLivenessHonorsFailureThreshold(t *testing.T) {
	var n atomic.Int32
	e, rt, clk := clockedVM(t, func(_ context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "live" {
			n.Add(1)
			return 1, nil
		}
		return 0, nil
	})
	pod := vmProbePod("lv", "uid-vm-live-th")
	pod.Spec.Containers[0].LivenessProbe = execProbe("live", 5, 1, 3, 5)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSamples(t, clk, &n, 1)
	if rt.Starts() != 1 {
		t.Fatalf("liveness restarted on the first failure: starts=%d samples=%d", rt.Starts(), n.Load())
	}
	clk.Advance(5 * time.Second)
	waitSamples(t, clk, &n, 2)
	if rt.Starts() != 1 {
		t.Fatalf("liveness restarted before failureThreshold: starts=%d samples=%d", rt.Starts(), n.Load())
	}
	clk.Advance(5 * time.Second)
	waitSamples(t, clk, &n, 3)
	if rt.Starts() != 1 {
		t.Fatalf("liveness restarted before the 10s backoff: starts=%d samples=%d", rt.Starts(), n.Load())
	}
	clk.Advance(10 * time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if n.Load() >= 3 && rt.Starts() >= 2 {
			if e.slots.Used() != 1 {
				t.Fatalf("slots=%d", e.slots.Used())
			}
			return
		}
		if rt.Starts() >= 2 && n.Load() < 3 {
			t.Fatalf("liveness restarted before 3 failures: starts=%d samples=%d", rt.Starts(), n.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("third liveness failure did not restart: starts=%d samples=%d", rt.Starts(), n.Load())
}

// One liveness success clears the failure streak. successThreshold other
// than 1 does not change that; kubelet requires 1 for liveness.
func TestVMLivenessSuccessClearsFailureStreak(t *testing.T) {
	var n atomic.Int32
	var pass atomic.Bool
	e, rt, clk := clockedVM(t, func(_ context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "live" {
			n.Add(1)
			if pass.Load() {
				return 0, nil
			}
			return 1, nil
		}
		return 0, nil
	})
	pod := vmProbePod("lv", "uid-vm-live-clear")
	pod.Spec.Containers[0].LivenessProbe = execProbe("live", 5, 1, 3, 5)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSamples(t, clk, &n, 1)
	clk.Advance(5 * time.Second)
	waitSamples(t, clk, &n, 2)
	if rt.Starts() != 1 {
		t.Fatalf("restarted while failing under the threshold: starts=%d", rt.Starts())
	}
	pass.Store(true)
	clk.Advance(5 * time.Second)
	waitSamples(t, clk, &n, 3)
	pass.Store(false)
	for i := int32(1); i <= 2; i++ {
		clk.Advance(5 * time.Second)
		waitSamples(t, clk, &n, 3+i)
		if rt.Starts() != 1 {
			t.Fatalf("success did not clear the liveness streak: starts=%d samples=%d", rt.Starts(), n.Load())
		}
	}
	clk.Advance(5 * time.Second)
	waitSamples(t, clk, &n, 6)
	if rt.Starts() != 1 {
		t.Fatalf("liveness restarted before the 10s backoff: starts=%d samples=%d", rt.Starts(), n.Load())
	}
	clk.Advance(10 * time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rt.Starts() >= 2 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("liveness did not restart after a cleared streak reached failureThreshold: starts=%d samples=%d", rt.Starts(), n.Load())
}

// initialDelaySeconds holds the first VM readiness sample. The container is
// not Ready during that wait.
func TestVMReadinessInitialDelay(t *testing.T) {
	var n atomic.Int32
	e, _, clk := clockedVM(t, func(_ context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "ready" {
			n.Add(1)
			return 0, nil
		}
		return 0, nil
	})
	pod := vmProbePod("rd", "uid-vm-ready-delay")
	p := execProbe("ready", 5, 1, 3, 1)
	p.InitialDelaySeconds = 30
	pod.Spec.Containers[0].ReadinessProbe = p
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	if !clk.WaitArmed(2 * time.Second) {
		t.Fatal("watch did not arm a timer for the initial delay")
	}
	if got := n.Load(); got != 0 {
		t.Fatalf("readiness ran %d times during initialDelaySeconds", got)
	}
	got, err := e.Get("default", "rd")
	if err != nil {
		t.Fatal(err)
	}
	main := containerStatus(got, "macos")
	if main == nil || main.Ready || podReady(got) {
		t.Fatalf("readiness stayed ready during initial delay: %+v", main)
	}
	c, ok := cond(got, corev1.PodReady)
	if !ok || c.Reason != "ReadinessProbe" || c.Message != "initial delay" {
		t.Fatalf("PodReady during initial delay %+v", c)
	}
	clk.Advance(5 * time.Second)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n.Load() != 0 {
			t.Fatalf("readiness ran after 5s of a 30s initial delay: samples=%d", n.Load())
		}
		if clk.armed() {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if n.Load() != 0 {
		t.Fatalf("readiness ran after 5s of a 30s initial delay: samples=%d", n.Load())
	}
	if !clk.armed() {
		t.Fatal("watch did not re-arm during the initial delay")
	}
	clk.Advance(30 * time.Second)
	waitSamples(t, clk, &n, 1)
	got, _ = e.Get("default", "rd")
	main = containerStatus(got, "macos")
	if main == nil || !main.Ready || !podReady(got) {
		t.Fatalf("readiness did not pass after the initial delay: samples=%d %+v", n.Load(), main)
	}
}

// timeoutSeconds bounds a VM liveness probe. A probe that does not return
// inside that budget is a failure, and failureThreshold 1 restarts the VM.
func TestVMLivenessTimeoutCountsAsFailure(t *testing.T) {
	var n atomic.Int32
	e, rt, clk := clockedVM(t, func(ctx context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "live" {
			n.Add(1)
			<-ctx.Done()
			return 0, ctx.Err()
		}
		return 0, nil
	})
	// The timeout is a real context deadline (minimum 1s). The period stays
	// on the injected clock so a second sample cannot start on the wall.
	e.cfg.ProbeInterval = time.Hour
	pod := vmProbePod("lv", "uid-vm-live-timeout")
	pod.Spec.Containers[0].LivenessProbe = execProbe("live", 30, 1, 1, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if n.Load() >= 1 && clk.armed() && rt.Starts() == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if n.Load() < 1 || !clk.armed() || rt.Starts() != 1 {
		t.Fatalf("liveness timeout did not arm backoff: samples=%d starts=%d armed=%v", n.Load(), rt.Starts(), clk.armed())
	}
	clk.Advance(10 * time.Second)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if rt.Starts() >= 2 {
			if e.slots.Used() != 1 {
				t.Fatalf("slots=%d", e.slots.Used())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("liveness timeout did not count as a failure: samples=%d starts=%d", n.Load(), rt.Starts())
}
