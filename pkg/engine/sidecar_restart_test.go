package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

// A hybrid sidecar that exits under RestartPolicy Always, or under OnFailure
// with a non-zero status, has to be started again. Until it is, the pod stays
// Running and not Ready and holds one of the two VM slots. The VM itself stays
// up: this is a container restart, not a second machine. OnFailure with exit 0
// is a completed container and is not restarted. A restart that cannot start
// the container fails the pod and releases the slot.

type restartingSidecar struct {
	sidecar.None
	mu          sync.Mutex
	boxes       map[string]*restartBox
	exitOnFirst map[string]int
	exitAlways  map[string]int
	failOn      map[string]int
}

type restartBox struct {
	creates int
	state   string
	code    int
	id      string
	started time.Time
}

func newRestartingSidecar() *restartingSidecar {
	return &restartingSidecar{
		boxes:       map[string]*restartBox{},
		exitOnFirst: map[string]int{},
		exitAlways:  map[string]int{},
		failOn:      map[string]int{},
	}
}

func (s *restartingSidecar) box(name string) *restartBox {
	b := s.boxes[name]
	if b == nil {
		b = &restartBox{}
		s.boxes[name] = b
	}
	return b
}

func (s *restartingSidecar) Create(_ context.Context, _ *corev1.Pod, c corev1.Container, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.box(c.Name)
	b.creates++
	if n := s.failOn[c.Name]; n > 0 && b.creates == n {
		return fmt.Errorf("docker start %s failed", c.Name)
	}
	b.id = fmt.Sprintf("mem://%s-%d", c.Name, b.creates)
	b.started = time.Now()
	if code, ok := s.exitOnFirst[c.Name]; ok && b.creates == 1 {
		b.state = "terminated"
		b.code = code
		return nil
	}
	if code, ok := s.exitAlways[c.Name]; ok {
		b.state = "terminated"
		b.code = code
		return nil
	}
	b.state = "running"
	b.code = 0
	return nil
}

func (s *restartingSidecar) List(context.Context, string, string) ([]sidecar.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.boxes) == 0 {
		return nil, nil
	}
	out := make([]sidecar.Status, 0, len(s.boxes))
	for name, b := range s.boxes {
		if b.state == "" {
			continue
		}
		out = append(out, sidecar.Status{
			Name:       name,
			ID:         b.id,
			State:      b.state,
			ExitCode:   b.code,
			StartedAt:  b.started,
			FinishedAt: b.started,
		})
	}
	return out, nil
}

func (s *restartingSidecar) kill(name string, code int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.box(name)
	b.state = "terminated"
	b.code = code
}

func (s *restartingSidecar) createsOf(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.boxes[name]
	if b == nil {
		return 0
	}
	return b.creates
}

func waitSidecarRestarts(t *testing.T, e *Engine, sc *restartingSidecar, podName, container string, creates int) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", podName)
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("sidecar restart failed the pod: %s", p.Status.Message)
		}
		side := (*corev1.ContainerStatus)(nil)
		if err == nil {
			side = containerStatus(p, container)
		}
		if err == nil && p.Status.Phase == corev1.PodRunning && podReady(p) &&
			sc.createsOf(container) >= creates && side != nil && side.RestartCount >= int32(creates-1) &&
			side.State.Running != nil {
			return p
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ := e.Get("default", podName)
	phase := corev1.PodPhase("missing")
	ready := false
	var side *corev1.ContainerStatus
	if p != nil {
		phase = p.Status.Phase
		ready = podReady(p)
		side = containerStatus(p, container)
	}
	t.Fatalf("sidecar %s was not restarted: creates=%d phase=%s ready=%v status=%+v", container, sc.createsOf(container), phase, ready, side)
	return nil
}

func TestHybridSidecarRestartsWhenPolicySaysSo(t *testing.T) {
	cases := []struct {
		name   string
		policy corev1.RestartPolicy
		code   int
	}{
		{name: "always-nonzero", policy: corev1.RestartPolicyAlways, code: 1},
		{name: "always-zero", policy: corev1.RestartPolicyAlways, code: 0},
		{name: "on-failure", policy: corev1.RestartPolicyOnFailure, code: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := fake.New()
			sc := newRestartingSidecar()
			e := sidecarEngine(t, rt, sc)
			if err := e.Create(context.Background(), hybridPod("hy", "uid-hy", tc.policy), Credentials{}); err != nil {
				t.Fatal(err)
			}
			waitRunningReady(t, e, "hy", nil)
			if sc.createsOf("log") != 1 {
				t.Fatalf("creates before exit = %d", sc.createsOf("log"))
			}
			sc.kill("log", tc.code)
			p := waitSidecarRestarts(t, e, sc, "hy", "log", 2)
			side := containerStatus(p, "log")
			if side.RestartCount != 1 || side.ContainerID != "mem://log-2" || !statusStarted(side) || !side.Ready {
				t.Fatalf("restarted sidecar %+v", side)
			}
			if side.LastTerminationState.Terminated == nil || side.LastTerminationState.Terminated.ExitCode != int32(tc.code) {
				t.Fatalf("last exit %+v, want %d", side.LastTerminationState.Terminated, tc.code)
			}
			main := containerStatus(p, "macos")
			if main == nil || main.RestartCount != 0 || !main.Ready || main.State.Running == nil {
				t.Fatalf("VM container %+v, sidecar restart must not restart the VM", main)
			}
			if e.slots.Used() != 1 || rt.Running() != 1 {
				t.Fatalf("slots=%d running=%d, sidecar restart changed the VM", e.slots.Used(), rt.Running())
			}
		})
	}
}

func TestHybridSidecarExitedBeforeReadyIsRestarted(t *testing.T) {
	rt := fake.New()
	sc := newRestartingSidecar()
	sc.exitOnFirst["log"] = 9
	e := sidecarEngine(t, rt, sc)
	if err := e.Create(context.Background(), hybridPod("hy", "uid-hy", corev1.RestartPolicyAlways), Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitSidecarRestarts(t, e, sc, "hy", "log", 2)
	side := containerStatus(p, "log")
	if side.ContainerID != "mem://log-2" || side.RestartCount != 1 {
		t.Fatalf("sidecar %+v", side)
	}
	if side.LastTerminationState.Terminated == nil || side.LastTerminationState.Terminated.ExitCode != 9 {
		t.Fatalf("last exit %+v", side.LastTerminationState.Terminated)
	}
	if e.slots.Used() != 1 || rt.Running() != 1 {
		t.Fatalf("slots=%d running=%d", e.slots.Used(), rt.Running())
	}
}

func TestHybridSidecarRestartLeavesSiblingRunning(t *testing.T) {
	rt := fake.New()
	sc := newRestartingSidecar()
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("hy", "uid-hy", corev1.RestartPolicyAlways)
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "metrics", Image: "busybox"})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitRunningReady(t, e, "hy", nil)
	sc.kill("log", 1)
	p := waitSidecarRestarts(t, e, sc, "hy", "log", 2)
	if sc.createsOf("metrics") != 1 {
		t.Fatalf("metrics creates=%d, healthy sidecar was restarted", sc.createsOf("metrics"))
	}
	metrics := containerStatus(p, "metrics")
	if metrics == nil || !metrics.Ready || metrics.RestartCount != 0 || metrics.State.Running == nil {
		t.Fatalf("metrics %+v", metrics)
	}
	if e.slots.Used() != 1 || rt.Running() != 1 {
		t.Fatalf("slots=%d running=%d", e.slots.Used(), rt.Running())
	}
}

func TestHybridSidecarOnFailureZeroDoesNotRestart(t *testing.T) {
	rt := fake.New()
	sc := newRestartingSidecar()
	e := sidecarEngine(t, rt, sc)
	if err := e.Create(context.Background(), hybridPod("hy", "uid-hy", corev1.RestartPolicyOnFailure), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitRunningReady(t, e, "hy", nil)
	sc.kill("log", 0)
	deadline := time.Now().Add(5 * time.Second)
	observed := false
	for time.Now().Before(deadline) {
		if sc.createsOf("log") != 1 {
			t.Fatalf("OnFailure exit 0 restarted the sidecar (%d creates)", sc.createsOf("log"))
		}
		p, err := e.Get("default", "hy")
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("OnFailure exit 0 failed the pod: %s", p.Status.Message)
		}
		if err == nil && p.Status.Phase == corev1.PodRunning && !podReady(p) {
			observed = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !observed {
		t.Fatal("OnFailure exit 0 did not clear PodReady")
	}
	hold := time.Now().Add(400 * time.Millisecond)
	for time.Now().Before(hold) {
		if n := sc.createsOf("log"); n != 1 {
			t.Fatalf("OnFailure exit 0 restarted the sidecar (%d creates)", n)
		}
		p, err := e.Get("default", "hy")
		if err != nil {
			t.Fatal(err)
		}
		if p.Status.Phase != corev1.PodRunning || podReady(p) {
			t.Fatalf("phase %s ready %v, completed sidecar must stay down", p.Status.Phase, podReady(p))
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, err := e.Get("default", "hy")
	if err != nil {
		t.Fatal(err)
	}
	side := containerStatus(p, "log")
	if side == nil || side.RestartCount != 0 || side.State.Terminated == nil || side.State.Terminated.ExitCode != 0 {
		t.Fatalf("sidecar %+v", side)
	}
	if e.slots.Used() != 1 || rt.Running() != 1 {
		t.Fatalf("slots=%d running=%d, completed sidecar must keep the VM", e.slots.Used(), rt.Running())
	}
}

func TestHybridSidecarRestartFailureReleasesSlot(t *testing.T) {
	rt := fake.New()
	sc := newRestartingSidecar()
	sc.failOn["log"] = 2
	e := sidecarEngine(t, rt, sc)
	if err := e.Create(context.Background(), hybridPod("hy", "uid-hy", corev1.RestartPolicyAlways), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitRunningReady(t, e, "hy", nil)
	sc.kill("log", 1)
	deadline := time.Now().Add(5 * time.Second)
	var got *corev1.Pod
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "hy")
		if err == nil && p.Status.Phase == corev1.PodFailed {
			got = p
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if got == nil {
		p, _ := e.Get("default", "hy")
		phase := corev1.PodPhase("missing")
		msg := ""
		if p != nil {
			phase = p.Status.Phase
			msg = p.Status.Message
		}
		t.Fatalf("restart error left the pod up: creates=%d phase=%s msg=%q running=%d slots=%d", sc.createsOf("log"), phase, msg, rt.Running(), e.slots.Used())
	}
	if !strings.Contains(got.Status.Message, "log") || !strings.Contains(got.Status.Message, "restart") {
		t.Fatalf("message %q", got.Status.Message)
	}
	waitVMReaped(t, e, rt, "hy", "uid-hy")
	if err := e.Create(context.Background(), samplePod("hy-next", "uid-hy-next"), Credentials{}); err != nil {
		t.Fatalf("slot not reusable after sidecar restart failure: %v", err)
	}
	if err := e.Create(context.Background(), samplePod("hy-cap", "uid-hy-cap"), Credentials{}); err != nil {
		t.Fatalf("second slot not free: %v", err)
	}
	err := e.Create(context.Background(), samplePod("hy-over", "uid-hy-over"), Credentials{})
	if !errors.Is(err, capacity.ErrVMCapacityExhausted) {
		if err == nil {
			_ = e.Delete(context.Background(), "default", "hy-over", 0)
		}
		t.Fatalf("cap after sidecar restart failure: %v", err)
	}
}

// A sidecar that exits on every start must not be started again on each
// probe tick. Kubelet waits CrashLoopBackOff: the first restart is immediate,
// then 10s, 20s, doubling to 5m. The VM stays up and keeps its slot.
func TestHybridSidecarCrashLoopBacksOff(t *testing.T) {
	rt := fake.New()
	sc := newRestartingSidecar()
	sc.exitAlways["log"] = 1
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("hy", "uid-hy", corev1.RestartPolicyAlways)
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "metrics", Image: "busybox"})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	armed := false
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "hy")
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("crash loop failed the pod: %s", p.Status.Message)
		}
		n := sc.createsOf("log")
		if err == nil && p.Status.Phase == corev1.PodRunning && n >= 2 {
			if n != 2 {
				t.Fatalf("sidecar was started %d times before backoff could apply", n)
			}
			armed = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !armed {
		t.Fatalf("sidecar was not restarted once: creates=%d", sc.createsOf("log"))
	}

	hold := time.Now().Add(400 * time.Millisecond)
	saw := false
	for time.Now().Before(hold) {
		if n := sc.createsOf("log"); n != 2 {
			t.Fatalf("sidecar restarted %d times with no backoff", n)
		}
		if sc.createsOf("metrics") != 1 {
			t.Fatalf("metrics creates=%d, the running sidecar was restarted too", sc.createsOf("metrics"))
		}
		p, err := e.Get("default", "hy")
		if err != nil {
			t.Fatal(err)
		}
		if p.Status.Phase != corev1.PodRunning || podReady(p) {
			t.Fatalf("phase %s ready %v, crash loop must keep the pod Running and not Ready", p.Status.Phase, podReady(p))
		}
		side := containerStatus(p, "log")
		if side != nil && side.State.Waiting != nil && side.State.Waiting.Reason == "CrashLoopBackOff" {
			if !strings.Contains(side.State.Waiting.Message, "back-off") || !strings.Contains(side.State.Waiting.Message, "log") {
				t.Fatalf("waiting message %q", side.State.Waiting.Message)
			}
			if side.Ready || side.RestartCount != 1 || side.State.Terminated != nil {
				t.Fatalf("status during backoff %+v", side)
			}
			last := side.LastTerminationState.Terminated
			if last == nil || last.ExitCode != 1 {
				t.Fatalf("last exit %+v", last)
			}
			metrics := containerStatus(p, "metrics")
			main := containerStatus(p, "macos")
			if metrics == nil || !metrics.Ready || metrics.RestartCount != 0 || metrics.State.Running == nil {
				t.Fatalf("metrics %+v", metrics)
			}
			if main == nil || main.RestartCount != 0 || main.State.Running == nil {
				t.Fatalf("VM container %+v", main)
			}
			if e.slots.Used() != 1 || rt.Running() != 1 {
				t.Fatalf("slots=%d running=%d, backoff must keep the VM slot", e.slots.Used(), rt.Running())
			}
			saw = true
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !saw {
		p, _ := e.Get("default", "hy")
		t.Fatalf("exited sidecar was not CrashLoopBackOff: %+v creates=%d", containerStatus(p, "log"), sc.createsOf("log"))
	}

	// The kubelet base delay is 10s. The 400ms hold above already elapsed, so
	// the next start is still several seconds away and must not be immediate.
	waited := time.Now()
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if n := sc.createsOf("log"); n >= 3 {
			if n != 3 {
				t.Fatalf("creates=%d, more than one start ran when the backoff elapsed", n)
			}
			if time.Since(waited) < 5*time.Second {
				t.Fatalf("third start after %s, backoff was shorter than the kubelet base", time.Since(waited))
			}
			goto doubled
		}
		if p, err := e.Get("default", "hy"); err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("crash loop failed the pod while backing off: %s", p.Status.Message)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("sidecar was not started after the backoff: creates=%d", sc.createsOf("log"))

doubled:
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n := sc.createsOf("log"); n != 3 {
			t.Fatalf("creates=%d, the delay did not grow after the second restart", n)
		}
		p, err := e.Get("default", "hy")
		if err != nil {
			t.Fatal(err)
		}
		if p.Status.Phase != corev1.PodRunning || podReady(p) || e.slots.Used() != 1 || rt.Running() != 1 {
			t.Fatalf("phase %s ready %v slots=%d running=%d", p.Status.Phase, podReady(p), e.slots.Used(), rt.Running())
		}
		side := containerStatus(p, "log")
		if side != nil && side.RestartCount == 2 && side.State.Waiting != nil && side.State.Waiting.Reason == "CrashLoopBackOff" {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ := e.Get("default", "hy")
	t.Fatalf("expected the longer backoff after the delayed restart: %+v", containerStatus(p, "log"))
}

func TestSidecarRestartBackoffMatchesKubelet(t *testing.T) {
	e := sidecarEngine(t, fake.New(), sidecar.None{})
	key := "pod/log"
	now := time.Now()
	if e.sidecarBackOff.IsInBackOffSince(key, now) {
		t.Fatal("the first exit is already backing off")
	}
	e.sidecarBackOff.Next(key, now)
	if d := e.sidecarBackOff.Get(key); d != 10*time.Second {
		t.Fatalf("base delay %s, kubelet starts at 10s", d)
	}
	if !e.sidecarBackOff.IsInBackOffSince(key, now) {
		t.Fatal("10s backoff did not arm")
	}
	if e.sidecarBackOff.IsInBackOffSince(key, now.Add(-10*time.Second)) {
		t.Fatal("10s backoff was still in force 10s after the exit")
	}
	e.sidecarBackOff.Next(key, now)
	if d := e.sidecarBackOff.Get(key); d != 20*time.Second {
		t.Fatalf("second delay %s, want 20s", d)
	}
	for i := 0; i < 8; i++ {
		e.sidecarBackOff.Next(key, now)
	}
	if d := e.sidecarBackOff.Get(key); d != 300*time.Second {
		t.Fatalf("cap %s, kubelet caps at 5m", d)
	}
	// client-go resets once the exit is more than twice the cap after the last update.
	stable := now.Add(2*300*time.Second + time.Second)
	if e.sidecarBackOff.IsInBackOffSince(key, stable) {
		t.Fatal("backoff did not reset after the container stayed up")
	}
	e.sidecarBackOff.Next(key, stable)
	if d := e.sidecarBackOff.Get(key); d != 10*time.Second {
		t.Fatalf("delay after a stable stretch %s, want 10s", d)
	}
}
