package engine

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

// ContainerStatus.Started becomes true once the container is running and,
// when a startup probe exists, once that probe has passed. Readiness does
// not clear it. A sidecar that has already run stays started after it exits.

func statusStarted(cs *corev1.ContainerStatus) bool {
	return cs != nil && cs.Started != nil && *cs.Started
}

func TestReadinessFailureKeepsContainerStarted(t *testing.T) {
	rt := fake.New()
	rt.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "unready" {
			return 1, nil
		}
		return 0, nil
	}
	e := sidecarEngine(t, rt, sidecar.None{})
	pod := samplePod("st", "uid-st")
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler:   corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"unready"}}},
		PeriodSeconds:  1,
		TimeoutSeconds: 1,
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "st", corev1.PodRunning)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "st")
		if err != nil || p.Status.Phase != corev1.PodRunning {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		main := containerStatus(p, "macos")
		if main == nil || main.Ready || main.State.Running == nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if !statusStarted(main) {
			t.Fatalf("readiness failure cleared Started: %+v", main)
		}
		if podReady(p) {
			t.Fatal("failing readiness left PodReady true")
		}
		return
	}
	p, _ := e.Get("default", "st")
	t.Fatalf("readiness did not fail while the container stayed running: %+v", p.Status.ContainerStatuses)
}

func TestStartupProbeGatesStarted(t *testing.T) {
	rt := fake.New()
	var pass atomic.Bool
	var boots atomic.Int32
	rt.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) == 0 {
			return 0, nil
		}
		switch req.Argv[0] {
		case "boot":
			boots.Add(1)
			if pass.Load() {
				return 0, nil
			}
			return 1, nil
		case "unready":
			return 1, nil
		default:
			return 0, nil
		}
	}
	e := sidecarEngine(t, rt, sidecar.None{})
	pod := samplePod("boot", "uid-boot")
	pod.Spec.Containers[0].StartupProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"boot"}}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 1000,
	}
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler:   corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"unready"}}},
		PeriodSeconds:  1,
		TimeoutSeconds: 1,
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "boot", corev1.PodRunning)

	deadline := time.Now().Add(5 * time.Second)
	sawHeld := false
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "boot")
		if err != nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if p.Status.Phase == corev1.PodFailed {
			t.Fatalf("startup probe failed the pod: %s", p.Status.Message)
		}
		main := containerStatus(p, "macos")
		if main != nil && boots.Load() > 0 && !main.Ready && main.State.Running != nil {
			if statusStarted(main) {
				t.Fatal("Started was true before the startup probe passed")
			}
			sawHeld = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !sawHeld {
		t.Fatal("did not observe a running container held not-started by its startup probe")
	}

	pass.Store(true)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "boot")
		if err != nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if p.Status.Phase == corev1.PodFailed {
			t.Fatalf("pod failed after startup passed: %s", p.Status.Message)
		}
		main := containerStatus(p, "macos")
		if main == nil || !statusStarted(main) || main.Ready || podReady(p) || main.State.Running == nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		return
	}
	p, _ := e.Get("default", "boot")
	main := containerStatus(p, "macos")
	t.Fatalf("startup passed but Started stayed false while not ready: %+v", main)
}

func TestTerminatedSidecarStaysStarted(t *testing.T) {
	rt := fake.New()
	sc := &exitSidecar{}
	e := sidecarEngine(t, rt, sc)
	if err := e.Create(context.Background(), hybridPod("hy", "uid-hy", corev1.RestartPolicyAlways), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitRunningReady(t, e, "hy", nil)
	running, err := e.Get("default", "hy")
	if err != nil {
		t.Fatal(err)
	}
	side := containerStatus(running, "log")
	if side == nil || !side.Ready || !statusStarted(side) {
		t.Fatalf("running sidecar %+v", side)
	}
	sc.set(sidecar.Status{
		Name: "log", ID: "mem://log", State: "terminated", ExitCode: 0, StartedAt: time.Now(),
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "hy")
		if err != nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if p.Status.Phase == corev1.PodFailed {
			t.Fatalf("zero exit failed the pod: %s", p.Status.Message)
		}
		side = containerStatus(p, "log")
		if side == nil || side.Ready || side.State.Terminated == nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if !statusStarted(side) {
			t.Fatalf("sidecar that had started reports Started false after exit: %+v", side)
		}
		main := containerStatus(p, "macos")
		if main == nil || !statusStarted(main) || !main.Ready {
			t.Fatalf("VM container %+v", main)
		}
		return
	}
	t.Fatal("sidecar did not leave Ready")
}
