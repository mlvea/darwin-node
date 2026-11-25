package engine

import (
	"context"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"

	corev1 "k8s.io/api/core/v1"
)

// Six liveness restarts used to fail the pod for the rest of its life.
// The pod stays up. The first restart waits 10s, then the delay doubles.
func TestLivenessBackoffDoesNotFailAtSix(t *testing.T) {
	var n atomic.Int32
	e, rt, clk := clockedVM(t, func(_ context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "dead" {
			n.Add(1)
			return 1, nil
		}
		return 0, nil
	})
	pod := vmProbePod("lv", "uid-live-backoff")
	pod.Spec.Containers[0].LivenessProbe = execProbe("dead", 3600, 1, 1, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "lv", corev1.PodRunning)
	waitSamples(t, clk, &n, 1)
	if rt.Starts() != 1 {
		t.Fatalf("starts=%d before the backoff elapsed, liveness restarted immediately", rt.Starts())
	}
	p, err := e.Get("default", "lv")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase == corev1.PodFailed {
		t.Fatalf("liveness failed the pod before any restart: %s", p.Status.Message)
	}
	if !strings.Contains(p.Status.Reason, "CrashLoop") && !strings.Contains(p.Status.Message, "back-off") {
		t.Fatalf("reason %q message %q, backoff was not visible", p.Status.Reason, p.Status.Message)
	}

	// 10s, 20s, 40s, 80s, 160s, 300s. Six restarts, none of them terminal.
	delays := []time.Duration{
		10 * time.Second,
		20 * time.Second,
		40 * time.Second,
		80 * time.Second,
		160 * time.Second,
		300 * time.Second,
	}
	for i, d := range delays {
		clk.Advance(d - time.Millisecond)
		time.Sleep(30 * time.Millisecond)
		if got := rt.Starts(); got != i+1 {
			t.Fatalf("starts=%d after %s minus a millisecond, restart %d came early", got, d, i+1)
		}
		clk.Advance(time.Millisecond)
		deadline := time.Now().Add(3 * time.Second)
		for rt.Starts() < i+2 && time.Now().Before(deadline) {
			time.Sleep(2 * time.Millisecond)
		}
		if got := rt.Starts(); got != i+2 {
			t.Fatalf("starts=%d, want %d after %s backoff", got, i+2, d)
		}
		p, err = e.Get("default", "lv")
		if err != nil {
			t.Fatal(err)
		}
		if p.Status.Phase == corev1.PodFailed {
			t.Fatalf("restart %d failed the pod: %s", i+1, p.Status.Message)
		}
		if e.slots.Used() != 1 {
			t.Fatalf("slots=%d during liveness backoff", e.slots.Used())
		}
		if i == len(delays)-1 {
			break
		}
		// The next failing sample is due after a 1ms probe wait. It arms
		// the following backoff. Advance only once that timer exists.
		if !clk.WaitArmed(2 * time.Second) {
			t.Fatal("watch did not arm the next probe")
		}
		clk.Advance(time.Millisecond)
		waitSamples(t, clk, &n, int32(i+2))
	}
}

// Ten minutes without a liveness restart resets the delay to 10s.
func TestLivenessBackoffResetsAfterHealthyRunning(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	var n atomic.Int32
	e, rt, clk := clockedVM(t, func(_ context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "dead" {
			n.Add(1)
			if fail.Load() {
				return 1, nil
			}
		}
		return 0, nil
	})
	pod := vmProbePod("lv", "uid-live-reset")
	pod.Spec.Containers[0].LivenessProbe = execProbe("dead", 3600, 1, 1, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "lv", corev1.PodRunning)
	waitSamples(t, clk, &n, 1)
	clk.Advance(10 * time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for rt.Starts() < 2 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if rt.Starts() != 2 {
		t.Fatalf("starts=%d, first restart did not happen at 10s", rt.Starts())
	}
	fail.Store(false)
	if !clk.WaitArmed(2 * time.Second) {
		t.Fatal("watch did not arm the probe after restart")
	}
	clk.Advance(time.Millisecond)
	waitSamples(t, clk, &n, 2)
	// Healthy for 10 minutes, then the rest of the 1h probe period.
	clk.Advance(10 * time.Minute)
	fail.Store(true)
	clk.Advance(3600*time.Second - 10*time.Minute)
	waitSamples(t, clk, &n, 3)
	if rt.Starts() != 2 {
		t.Fatalf("starts=%d, failure after 10m healthy restarted immediately", rt.Starts())
	}
	clk.Advance(10*time.Second - time.Millisecond)
	time.Sleep(30 * time.Millisecond)
	if rt.Starts() != 2 {
		t.Fatalf("starts=%d, backoff did not reset to 10s", rt.Starts())
	}
	clk.Advance(time.Millisecond)
	deadline = time.Now().Add(3 * time.Second)
	for rt.Starts() < 3 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if rt.Starts() != 3 {
		t.Fatalf("starts=%d, reset backoff did not restart at 10s", rt.Starts())
	}
	if p, err := e.Get("default", "lv"); err != nil || p.Status.Phase == corev1.PodFailed {
		msg := ""
		if p != nil {
			msg = p.Status.Message
		}
		t.Fatalf("pod failed after the reset restart: %v %s", err, msg)
	}
}
