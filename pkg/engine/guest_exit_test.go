package engine

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

func waitNoGuestExit(t *testing.T, e *Engine, key string) {
	t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for time.Now().Before(deadline) {
		e.mu.RLock()
		rec := e.pods[key]
		e.mu.RUnlock()
		if rec != nil {
			rec.mu.Lock()
			got := rec.guestExit
			rec.mu.Unlock()
			if got != nil {
				t.Fatalf("guest exit %d was recorded without a code", *got)
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitGuestExit(t *testing.T, e *Engine, key string, want int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.RLock()
		rec := e.pods[key]
		e.mu.RUnlock()
		if rec != nil {
			rec.mu.Lock()
			got := rec.guestExit
			rec.mu.Unlock()
			if got != nil && *got == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("guest exit %d was not recorded", want)
}

func terminated(t *testing.T, p *corev1.Pod) *corev1.ContainerStateTerminated {
	t.Helper()
	if len(p.Status.ContainerStatuses) == 0 || p.Status.ContainerStatuses[0].State.Terminated == nil {
		t.Fatalf("container not terminated: %+v", p.Status.ContainerStatuses)
	}
	return p.Status.ContainerStatuses[0].State.Terminated
}

func TestStoppedVMPublishesGuestExitCode(t *testing.T) {
	rt := fake.New()
	e := sidecarEngine(t, rt, sidecar.None{})
	pod := samplePod("die", "uid-guest-exit")
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "die", corev1.PodRunning)
	code := int32(137)
	rt.ReportExit(guest.ExitReport{ExitCode: &code, Reason: "OOMKilled"})
	waitGuestExit(t, e, "default/die", 137)

	p, err := e.Get("default", "die")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase != corev1.PodRunning || len(p.Status.ContainerStatuses) == 0 || p.Status.ContainerStatuses[0].State.Terminated != nil {
		t.Fatalf("exit report failed the running pod: phase=%s %+v", p.Status.Phase, p.Status.ContainerStatuses)
	}
	if err := podMachine(t, e, "default/die").Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	p = waitPhase(t, e, "default", "die", corev1.PodFailed)
	term := terminated(t, p)
	if term.ExitCode != 137 || term.Reason != "OOMKilled" {
		t.Fatalf("exit %d reason %q, guest exit report was not published (msg=%q)", term.ExitCode, term.Reason, p.Status.Message)
	}
	if !strings.Contains(p.Status.Message, "Stopped") {
		t.Fatalf("message %q, want the VM stop", p.Status.Message)
	}
}

func TestGuestExitZeroIsPublished(t *testing.T) {
	rt := fake.New()
	e := sidecarEngine(t, rt, sidecar.None{})
	pod := samplePod("die", "uid-guest-zero")
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "die", corev1.PodRunning)
	zero := int32(0)
	rt.ReportExit(guest.ExitReport{ExitCode: &zero, Reason: "Completed"})
	waitGuestExit(t, e, "default/die", 0)
	if err := podMachine(t, e, "default/die").Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "die", corev1.PodSucceeded)
	term := terminated(t, p)
	if term.ExitCode != 0 || term.Reason != "Completed" {
		t.Fatalf("exit %d reason %q, a guest exit of 0 was dropped", term.ExitCode, term.Reason)
	}
}

func TestGuestExitWithoutCodeStaysOne(t *testing.T) {
	rt := fake.New()
	e := sidecarEngine(t, rt, sidecar.None{})
	if err := e.Create(context.Background(), samplePod("die", "uid-guest-none"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "die", corev1.PodRunning)
	rt.ReportExit(guest.ExitReport{Reason: "Error"})
	waitNoGuestExit(t, e, "default/die")
	if err := podMachine(t, e, "default/die").Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "die", corev1.PodFailed)
	term := terminated(t, p)
	if term.ExitCode != 1 || term.Reason != "VMStopped" {
		t.Fatalf("exit %d reason %q, missing guest code did not fall back to 1", term.ExitCode, term.Reason)
	}
}

func TestGuestExitBadReasonKeepsHostReason(t *testing.T) {
	rt := fake.New()
	e := sidecarEngine(t, rt, sidecar.None{})
	pod := samplePod("die", "uid-guest-bad-reason")
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "die", corev1.PodRunning)
	code := int32(9)
	rt.ReportExit(guest.ExitReport{ExitCode: &code, Reason: "not a reason"})
	waitGuestExit(t, e, "default/die", 9)
	if err := podMachine(t, e, "default/die").Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "die", corev1.PodFailed)
	term := terminated(t, p)
	if term.ExitCode != 9 || term.Reason != "VMStopped" {
		t.Fatalf("exit %d reason %q, bad guest reason replaced the host reason", term.ExitCode, term.Reason)
	}
}

func TestGuestExitCodeIsClamped(t *testing.T) {
	cases := []struct {
		in   int32
		want int32
	}{
		{1000, 255},
		{256, 255},
		{-1, 0},
		{137, 137},
		{0, 0},
	}
	for _, tc := range cases {
		rt := fake.New()
		e := sidecarEngine(t, rt, sidecar.None{})
		name := fmt.Sprintf("die-%d", tc.in)
		pod := samplePod(name, "uid-clamp-"+name)
		pod.Spec.RestartPolicy = corev1.RestartPolicyNever
		if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
			t.Fatal(err)
		}
		waitPhase(t, e, "default", name, corev1.PodRunning)
		code := tc.in
		rt.ReportExit(guest.ExitReport{ExitCode: &code, Reason: "Error"})
		waitGuestExit(t, e, "default/"+name, tc.want)
		if err := podMachine(t, e, "default/"+name).Stop(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
		phase := corev1.PodFailed
		if tc.want == 0 {
			phase = corev1.PodSucceeded
		}
		p := waitPhase(t, e, "default", name, phase)
		term := terminated(t, p)
		if term.ExitCode != tc.want {
			t.Fatalf("guest exit %d published as %d, want %d", tc.in, term.ExitCode, tc.want)
		}
	}
}

func TestNoGuestExitStaysOne(t *testing.T) {
	rt := fake.New()
	e := sidecarEngine(t, rt, sidecar.None{})
	if err := e.Create(context.Background(), samplePod("die", "uid-no-guest-exit"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "die", corev1.PodRunning)
	if err := podMachine(t, e, "default/die").Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "die", corev1.PodFailed)
	term := terminated(t, p)
	if term.ExitCode != 1 || term.Reason != "VMStopped" {
		t.Fatalf("exit %d reason %q, a VM with no guest report did not stay at 1", term.ExitCode, term.Reason)
	}
}

// A liveness restart starts a new container. The previous guest exit does
// not stay on it, so the next unexpected stop falls back to 1.
func TestLivenessRestartDropsGuestExit(t *testing.T) {
	var n atomic.Int32
	var fail atomic.Bool
	e, rt, clk := clockedVM(t, func(_ context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "live" {
			n.Add(1)
			if fail.Load() {
				return 1, nil
			}
		}
		return 0, nil
	})
	pod := vmProbePod("lv", "uid-guest-exit-restart")
	pod.Spec.Containers[0].LivenessProbe = execProbe("live", 5, 1, 1, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSamples(t, clk, &n, 1)
	code := int32(137)
	rt.ReportExit(guest.ExitReport{ExitCode: &code, Reason: "OOMKilled"})
	waitGuestExit(t, e, "default/lv", 137)
	fail.Store(true)
	clk.Advance(5 * time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.RLock()
		rec := e.pods["default/lv"]
		e.mu.RUnlock()
		var kept *int32
		if rec != nil {
			rec.mu.Lock()
			kept = rec.guestExit
			rec.mu.Unlock()
		}
		if rt.Starts() >= 2 && clk.armed() && kept == nil {
			break
		}
		if rt.Starts() >= 2 && clk.armed() && kept != nil {
			t.Fatalf("liveness restart kept guest exit %d", *kept)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if rt.Starts() < 2 {
		t.Fatalf("liveness did not restart: starts=%d samples=%d", rt.Starts(), n.Load())
	}
	e.mu.RLock()
	rec := e.pods["default/lv"]
	e.mu.RUnlock()
	rec.mu.Lock()
	kept := rec.guestExit
	rec.mu.Unlock()
	if kept != nil {
		t.Fatalf("liveness restart kept guest exit %d", *kept)
	}
	if err := podMachine(t, e, "default/lv").Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Second)
	p := waitPhase(t, e, "default", "lv", corev1.PodFailed)
	term := terminated(t, p)
	if term.ExitCode != 1 || term.Reason != "VMStopped" {
		t.Fatalf("exit %d reason %q after restart, previous guest code was kept", term.ExitCode, term.Reason)
	}
}
