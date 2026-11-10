package engine

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

// A hybrid sidecar that is not running clears PodReady. RestartPolicy Never
// plus a non-zero exit fails the pod: that container will not be restarted,
// and the VM would otherwise keep one of the two slots. Always and OnFailure
// mean restart, which this node does not do; those pods stay Running and not
// Ready. A zero exit is success and does not fail the pod.

func sidecarEngine(t *testing.T, rt runtime.Runtime, sc sidecar.Runtime) *Engine {
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
	cfg.ProbeInterval = 20 * time.Millisecond
	e := New(cfg, slots, rt, sc, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)
	return e
}

func hybridPod(name, uid string, policy corev1.RestartPolicy) *corev1.Pod {
	p := samplePod(name, uid)
	p.Spec.RestartPolicy = policy
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "log", Image: "busybox"})
	return p
}

func withPassingReadiness(pod *corev1.Pod) *corev1.Pod {
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler:   corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}},
		PeriodSeconds:  1,
		TimeoutSeconds: 1,
	}
	return pod
}

func countExecs(rt *fake.Runtime) *atomic.Int32 {
	var n atomic.Int32
	rt.ExecFn = func(context.Context, guest.ExecReq, io.Reader, io.Writer, io.Writer) (int, error) {
		n.Add(1)
		return 0, nil
	}
	return &n
}

func cond(p *corev1.Pod, typ corev1.PodConditionType) (corev1.PodCondition, bool) {
	for _, c := range p.Status.Conditions {
		if c.Type == typ {
			return c, true
		}
	}
	return corev1.PodCondition{}, false
}

func containerStatus(p *corev1.Pod, name string) *corev1.ContainerStatus {
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == name {
			return &p.Status.ContainerStatuses[i]
		}
	}
	return nil
}

// exitSidecar is a host sidecar whose status the test replaces after Create.
type exitSidecar struct {
	sidecar.None
	mu sync.Mutex
	st sidecar.Status
}

func (s *exitSidecar) Create(_ context.Context, _ *corev1.Pod, c corev1.Container, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Name == "" {
		s.st.Name = c.Name
	}
	if s.st.State == "" {
		s.st.State = "running"
	}
	if s.st.ID == "" {
		s.st.ID = "mem://" + c.Name
	}
	if s.st.StartedAt.IsZero() {
		s.st.StartedAt = time.Now()
	}
	return nil
}

func (s *exitSidecar) List(context.Context, string, string) ([]sidecar.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Name == "" {
		return nil, nil
	}
	return []sidecar.Status{s.st}, nil
}

func (s *exitSidecar) set(st sidecar.Status) {
	s.mu.Lock()
	s.st = st
	s.mu.Unlock()
}

func waitRunningReady(t *testing.T, e *Engine, name string, execs *atomic.Int32) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", name)
		probed := execs == nil || execs.Load() > 0
		if err == nil && p.Status.Phase == corev1.PodRunning && probed && podReady(p) {
			return p
		}
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("pod failed early: %s", p.Status.Message)
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ := e.Get("default", name)
	t.Fatalf("pod %s did not become Ready: %+v", name, p.Status)
	return nil
}

func assertSidecarNotReady(t *testing.T, p *corev1.Pod, code int32) {
	t.Helper()
	if p.Status.Phase != corev1.PodRunning {
		t.Fatalf("phase %s msg=%q, sidecar exit failed the pod", p.Status.Phase, p.Status.Message)
	}
	if podReady(p) {
		t.Fatal("exited sidecar left PodReady true")
	}
	c, ok := cond(p, corev1.PodReady)
	if !ok || c.Status != corev1.ConditionFalse || c.Reason != "SidecarNotRunning" || !strings.Contains(c.Message, "log") {
		t.Fatalf("PodReady condition %+v", c)
	}
	cr, ok := cond(p, corev1.ContainersReady)
	if !ok || cr.Status != corev1.ConditionFalse || cr.Reason != "SidecarNotRunning" {
		t.Fatalf("ContainersReady condition %+v", cr)
	}
	main := containerStatus(p, "macos")
	if main == nil || !main.Ready {
		t.Fatalf("VM container readiness = %+v, want Ready", main)
	}
	side := containerStatus(p, "log")
	if side == nil || side.Ready || side.State.Terminated == nil || side.State.Terminated.ExitCode != code {
		t.Fatalf("sidecar status %+v", side)
	}
}

func TestHybridSidecarExitClearsPodReady(t *testing.T) {
	cases := []struct {
		name   string
		policy corev1.RestartPolicy
		code   int
	}{
		{name: "always", policy: corev1.RestartPolicyAlways, code: 1},
		{name: "on-failure", policy: corev1.RestartPolicyOnFailure, code: 1},
		{name: "never-zero", policy: corev1.RestartPolicyNever, code: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := fake.New()
			execs := countExecs(rt)
			sc := &exitSidecar{}
			sc.set(sidecar.Status{Name: "log", State: "terminated", ExitCode: tc.code, ID: "mem://log"})
			e := sidecarEngine(t, rt, sc)
			pod := withPassingReadiness(hybridPod("hy", "uid-hy", tc.policy))
			if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().Add(5 * time.Second)
			var got *corev1.Pod
			for time.Now().Before(deadline) {
				p, err := e.Get("default", "hy")
				if err == nil && p.Status.Phase == corev1.PodFailed {
					t.Fatalf("policy %s exit %d failed the pod: %s", tc.policy, tc.code, p.Status.Message)
				}
				if err == nil && p.Status.Phase == corev1.PodRunning && execs.Load() > 0 {
					if podReady(p) {
						t.Fatal("exited sidecar left PodReady true after the VM readiness probe passed")
					}
					got = p
					break
				}
				time.Sleep(15 * time.Millisecond)
			}
			if got == nil {
				p, _ := e.Get("default", "hy")
				t.Fatalf("pod did not settle after probe: phase=%s ready=%v execs=%d", p.Status.Phase, podReady(p), execs.Load())
			}
			assertSidecarNotReady(t, got, int32(tc.code))
			if e.slots.Used() != 1 {
				t.Fatalf("slots=%d, not-ready sidecar must still hold the VM", e.slots.Used())
			}
			if rt.Running() != 1 {
				t.Fatalf("running VMs=%d, want the VM kept", rt.Running())
			}
		})
	}
}

func TestHybridSidecarExitAfterRunningClearsPodReady(t *testing.T) {
	rt := fake.New()
	sc := &exitSidecar{}
	e := sidecarEngine(t, rt, sc)
	if err := e.Create(context.Background(), hybridPod("hy", "uid-hy", corev1.RestartPolicyAlways), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitRunningReady(t, e, "hy", nil)
	sc.set(sidecar.Status{Name: "log", ID: "mem://log", State: "terminated", ExitCode: 1})
	deadline := time.Now().Add(5 * time.Second)
	var got *corev1.Pod
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "hy")
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("Always must not fail the pod: %s", p.Status.Message)
		}
		if err == nil && p.Status.Phase == corev1.PodRunning && !podReady(p) {
			got = p
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("PodReady stayed true after the sidecar exited")
	}
	assertSidecarNotReady(t, got, 1)
	if e.slots.Used() != 1 || rt.Running() != 1 {
		t.Fatalf("slots=%d running=%d, exited Always sidecar must keep the VM", e.slots.Used(), rt.Running())
	}
}

func TestHybridSidecarNonZeroExitOnNeverFailsPod(t *testing.T) {
	rt := fake.New()
	sc := &exitSidecar{}
	sc.set(sidecar.Status{Name: "log", State: "terminated", ExitCode: 1, ID: "mem://log", Error: "boom"})
	e := sidecarEngine(t, rt, sc)
	if err := e.Create(context.Background(), hybridPod("hy", "uid-hy", corev1.RestartPolicyNever), Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var got *corev1.Pod
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "hy")
		if err == nil && p.Status.Phase == corev1.PodRunning {
			t.Fatalf("RestartPolicyNever left the pod Running after sidecar exit 1: ready=%v", podReady(p))
		}
		if err == nil && p.Status.Phase == corev1.PodFailed {
			got = p
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if got == nil {
		p, _ := e.Get("default", "hy")
		t.Fatalf("pod did not fail: phase=%s msg=%q", p.Status.Phase, p.Status.Message)
	}
	if !strings.Contains(got.Status.Message, "sidecar log exited 1") || !strings.Contains(got.Status.Message, "boom") {
		t.Fatalf("message %q", got.Status.Message)
	}
	if podReady(got) {
		t.Fatal("failed pod still PodReady")
	}
	waitVMReaped(t, e, rt, "hy", "uid-hy")
	if err := e.Create(context.Background(), samplePod("hy-next", "uid-hy-next"), Credentials{}); err != nil {
		t.Fatalf("slot not reusable: %v", err)
	}
	waitPhase(t, e, "default", "hy-next", corev1.PodRunning)
	_ = e.Delete(context.Background(), "default", "hy-next", 0)
}

func TestHybridSidecarNonZeroExitAfterRunningFailsPod(t *testing.T) {
	rt := fake.New()
	sc := &exitSidecar{}
	e := sidecarEngine(t, rt, sc)
	if err := e.Create(context.Background(), hybridPod("hy", "uid-hy", corev1.RestartPolicyNever), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitRunningReady(t, e, "hy", nil)
	sc.set(sidecar.Status{Name: "log", ID: "mem://log", State: "terminated", ExitCode: 7})
	p := waitPhase(t, e, "default", "hy", corev1.PodFailed)
	if !strings.Contains(p.Status.Message, "sidecar log exited 7") {
		t.Fatalf("message %q", p.Status.Message)
	}
	if podReady(p) {
		t.Fatal("failed pod still PodReady")
	}
	waitVMReaped(t, e, rt, "hy", "uid-hy")
}

func TestHybridSidecarStaysReadyWhileRunning(t *testing.T) {
	rt := fake.New()
	execs := countExecs(rt)
	e := sidecarEngine(t, rt, sidecar.NewMemory())
	pod := withPassingReadiness(hybridPod("hy", "uid-hy", corev1.RestartPolicyAlways))
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitRunningReady(t, e, "hy", execs)
	main := containerStatus(p, "macos")
	side := containerStatus(p, "log")
	if main == nil || !main.Ready || side == nil || !side.Ready || side.State.Running == nil {
		t.Fatalf("statuses %+v", p.Status.ContainerStatuses)
	}
	if e.slots.Used() != 1 || rt.Running() != 1 {
		t.Fatalf("slots=%d running=%d", e.slots.Used(), rt.Running())
	}
}
