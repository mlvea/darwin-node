package engine

import (
	"context"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

func testEngineRuntime(t *testing.T, rt runtime.Runtime) *Engine {
	t.Helper()
	leakcheck.Check(t)
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	return New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
}

func TestAgentRedialsAfterSessionDrop(t *testing.T) {
	leakcheck.Check(t)
	rt := fake.New()
	e := testEngineRuntime(t, rt)
	pod := samplePod("redial", "uid-redial")
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Delete(context.Background(), "default", pod.Name, 0) })
	p := waitPhase(t, e, "default", pod.Name, corev1.PodRunning)
	if !podReady(p) {
		t.Fatalf("not ready before the drop: %+v", p.Status.Conditions)
	}

	rt.DropAgent()

	deadline := time.Now().Add(5 * time.Second)
	var dials int
	var ready bool
	for time.Now().Before(deadline) {
		dials = rt.AgentDials()
		got, err := e.Get("default", pod.Name)
		if err == nil && dials >= 2 && got.Status.Phase == corev1.PodRunning && podReady(got) {
			ready = true
			if err := e.ExecInVM(context.Background(), "default", pod.Name, []string{"/bin/echo", "ok"}, nil); err == nil {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session was not redialed: dials=%d ready=%v", dials, ready)
}

func TestAgentRedialBudgetMarksNotReady(t *testing.T) {
	leakcheck.Check(t)
	oldBack, oldMax, oldBudget := agentRedialBackoff, agentRedialMax, agentRedialBudget
	agentRedialBackoff = 10 * time.Millisecond
	agentRedialMax = 20 * time.Millisecond
	agentRedialBudget = 150 * time.Millisecond
	t.Cleanup(func() {
		agentRedialBackoff = oldBack
		agentRedialMax = oldMax
		agentRedialBudget = oldBudget
	})

	rt := fake.New()
	rt.FailDialAfter = 1
	e := testEngineRuntime(t, rt)
	pod := samplePod("lost", "uid-lost")
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Delete(context.Background(), "default", pod.Name, 0) })
	waitPhase(t, e, "default", pod.Name, corev1.PodRunning)
	rt.DropAgent()

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		got, err := e.Get("default", pod.Name)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status.Phase == corev1.PodRunning && !podReady(got) && podReadyReason(got) == guestAgentLostReason {
			if len(got.Status.ContainerStatuses) == 0 || got.Status.ContainerStatuses[0].Started == nil || !*got.Status.ContainerStatuses[0].Started {
				t.Fatalf("Started cleared while the VM was up: %+v", got.Status.ContainerStatuses)
			}
			return
		}
		if got.Status.Phase == corev1.PodFailed {
			t.Fatalf("pod failed instead of staying Running: %s", got.Status.Message)
		}
		time.Sleep(20 * time.Millisecond)
	}
	got, _ := e.Get("default", pod.Name)
	t.Fatalf("pod stayed ready after redial failed: phase=%s reason=%s", got.Status.Phase, podReadyReason(got))
}

func podReadyReason(p *corev1.Pod) string {
	if p == nil {
		return ""
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Reason
		}
	}
	return ""
}
