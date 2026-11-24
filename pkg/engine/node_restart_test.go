package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func restartEngine(t *testing.T, cache string, rt runtime.Runtime) *Engine {
	t.Helper()
	leakcheck.Check(t)
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = cache
	cfg.AgentReadyTimeout = 2 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func plantPodDir(t *testing.T, cache, uid string) {
	t.Helper()
	dir := filepath.Join(cache, "pods", uid, "overlay")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte("SECRET-WORK"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runningStatus(restart int32) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:         "macos",
			Ready:        true,
			RestartCount: restart,
			State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()},
			},
		}},
	}
}

// requireNotRunning fails as soon as a pod that must stay down is booted.
func requireNotRunning(t *testing.T, e *Engine, rt *fake.Runtime, name, wantReason string, wantPhase corev1.PodPhase) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last *corev1.Pod
	for time.Now().Before(deadline) {
		p, err := e.Get("default", name)
		if err != nil {
			t.Fatal(err)
		}
		last = p
		if p.Status.Phase == corev1.PodRunning || rt.Starts() > 0 {
			t.Fatalf("interrupted pod %s was started again: phase %s reason %q starts=%d", name, p.Status.Phase, p.Status.Reason, rt.Starts())
		}
		if p.Status.Phase == wantPhase && (wantReason == "" || p.Status.Reason == wantReason) {
			if e.slots.Used() != 0 {
				t.Fatalf("slot held for a pod that was not started: used=%d", e.slots.Used())
			}
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last == nil {
		t.Fatalf("pod %s missing, want %s %s", name, wantPhase, wantReason)
	}
	t.Fatalf("pod %s phase %s reason %q, want %s %s (starts=%d)", name, last.Status.Phase, last.Status.Reason, wantPhase, wantReason, rt.Starts())
	return last
}

func TestProcessRestartDoesNotRerunNeverPod(t *testing.T) {
	cache := t.TempDir()
	const uid = "uid-never"
	plantPodDir(t, cache, uid)
	rt := fake.New()
	e := restartEngine(t, cache, rt)
	if _, err := os.Stat(filepath.Join(cache, "pods", uid)); !os.IsNotExist(err) {
		t.Fatalf("startup left the previous pod disk: %v", err)
	}
	pod := samplePod("job", uid)
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	pod.Status = runningStatus(0)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := requireNotRunning(t, e, rt, "job", "NodeRestarted", corev1.PodFailed)
	cs := containerStatus(p, "macos")
	if cs == nil || cs.State.Terminated == nil || cs.State.Terminated.ExitCode == 0 {
		t.Fatalf("container status %+v, want a non-zero terminated state", cs)
	}
	if cs.RestartCount != 0 {
		t.Fatalf("restartCount %d, a Never pod was counted as restarted", cs.RestartCount)
	}
}

func TestProcessRestartMarksNeverPodFromAPIStatus(t *testing.T) {
	// The disk is already gone. Virtual-kubelet still passes the pod
	// whose status says it was Running.
	rt := fake.New()
	e := restartEngine(t, t.TempDir(), rt)
	pod := samplePod("job", "uid-never-status")
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	pod.Status = runningStatus(1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := requireNotRunning(t, e, rt, "job", "NodeRestarted", corev1.PodFailed)
	if cs := containerStatus(p, "macos"); cs == nil || cs.RestartCount != 1 {
		t.Fatalf("restartCount %+v, want the previous count left as it was", cs)
	}
}

func TestProcessRestartCountsAlwaysPod(t *testing.T) {
	cache := t.TempDir()
	const uid = "uid-always"
	plantPodDir(t, cache, uid)
	rt := fake.New()
	e := restartEngine(t, cache, rt)
	pod := samplePod("web", uid)
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	pod.Status = runningStatus(2)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "web", corev1.PodRunning)
	cs := containerStatus(p, "macos")
	if cs == nil || cs.RestartCount != 3 {
		t.Fatalf("restartCount %+v, want 3 after the node restart", cs)
	}
	if rt.Starts() != 1 {
		t.Fatalf("VM starts %d, want one restart boot", rt.Starts())
	}
}

func TestProcessRestartCountsOnFailurePod(t *testing.T) {
	cache := t.TempDir()
	const uid = "uid-onfail"
	plantPodDir(t, cache, uid)
	rt := fake.New()
	e := restartEngine(t, cache, rt)
	pod := samplePod("batch", uid)
	pod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
	pod.Status = runningStatus(0)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "batch", corev1.PodRunning)
	cs := containerStatus(p, "macos")
	if cs == nil || cs.RestartCount != 1 {
		t.Fatalf("restartCount %+v, want 1", cs)
	}
}

func TestProcessRestartKeepsSucceededPod(t *testing.T) {
	cache := t.TempDir()
	const uid = "uid-ok"
	plantPodDir(t, cache, uid)
	rt := fake.New()
	e := restartEngine(t, cache, rt)
	pod := samplePod("job", uid)
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	pod.Status = corev1.PodStatus{
		Phase:   corev1.PodSucceeded,
		Reason:  "Completed",
		Message: "workload exited 0",
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "macos",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Reason: "Completed",
			}},
		}},
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := requireNotRunning(t, e, rt, "job", "Completed", corev1.PodSucceeded)
	cs := containerStatus(p, "macos")
	if cs == nil || cs.State.Terminated == nil || cs.State.Terminated.ExitCode != 0 {
		t.Fatalf("container %+v", cs)
	}
}

func TestProcessRestartKeepsFailedPod(t *testing.T) {
	cache := t.TempDir()
	const uid = "uid-failed"
	plantPodDir(t, cache, uid)
	rt := fake.New()
	e := restartEngine(t, cache, rt)
	pod := samplePod("job", uid)
	pod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure
	pod.Status = corev1.PodStatus{
		Phase:   corev1.PodFailed,
		Reason:  "Error",
		Message: "workload exited 9",
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "macos",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 9, Reason: "Error",
			}},
		}},
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := requireNotRunning(t, e, rt, "job", "Error", corev1.PodFailed)
	cs := containerStatus(p, "macos")
	if cs == nil || cs.State.Terminated == nil || cs.State.Terminated.ExitCode != 9 {
		t.Fatalf("container %+v, want exit 9 kept", cs)
	}
}

func TestFreshPodAfterRestartStillStarts(t *testing.T) {
	cache := t.TempDir()
	plantPodDir(t, cache, "uid-old")
	rt := fake.New()
	e := restartEngine(t, cache, rt)
	pod := samplePod("new", "uid-new")
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "new", corev1.PodRunning)
	if rt.Starts() != 1 {
		t.Fatalf("starts %d", rt.Starts())
	}
}
