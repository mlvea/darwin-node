package engine

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// cmdSidecar is a host container whose exec probes the test can pass, fail,
// or hold open. Create leaves it running. StopContainer records the kill.
type cmdSidecar struct {
	sidecar.None
	mu      sync.Mutex
	st      sidecar.Status
	creates atomic.Int32
	stops   atomic.Int32
	execs   atomic.Int32
	stopErr error
	fail    map[string]*atomic.Bool
	count   map[string]*atomic.Int32
	bad     map[string]*atomic.Int32
	block   map[string]bool
	entered chan struct{}
	once    sync.Once
}

func newCmdSidecar() *cmdSidecar {
	return &cmdSidecar{
		fail:    map[string]*atomic.Bool{},
		count:   map[string]*atomic.Int32{},
		bad:     map[string]*atomic.Int32{},
		block:   map[string]bool{},
		entered: make(chan struct{}),
	}
}

func (s *cmdSidecar) gate(cmd string) *atomic.Bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail[cmd] == nil {
		s.fail[cmd] = &atomic.Bool{}
	}
	return s.fail[cmd]
}

func (s *cmdSidecar) calls(cmd string) *atomic.Int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.count[cmd] == nil {
		s.count[cmd] = &atomic.Int32{}
	}
	return s.count[cmd]
}

// failures counts execs that already returned an error. calls counts starts,
// including a probe that has not decided yet.
func (s *cmdSidecar) failures(cmd string) *atomic.Int32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bad[cmd] == nil {
		s.bad[cmd] = &atomic.Int32{}
	}
	return s.bad[cmd]
}

func (s *cmdSidecar) Create(_ context.Context, _ *corev1.Pod, c corev1.Container, _ string) error {
	s.creates.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st = sidecar.Status{Name: c.Name, ID: "mem://" + c.Name, State: "running", StartedAt: time.Now()}
	return nil
}

func (s *cmdSidecar) StopContainer(context.Context, string, string, string) error {
	if s.stopErr != nil {
		return s.stopErr
	}
	s.stops.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.State = "terminated"
	s.st.ExitCode = 137
	s.st.FinishedAt = time.Now()
	s.st.Error = "killed"
	return nil
}

func (s *cmdSidecar) List(context.Context, string, string) ([]sidecar.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Name == "" {
		return nil, nil
	}
	return []sidecar.Status{s.st}, nil
}

func (s *cmdSidecar) Exec(ctx context.Context, _, _, _ string, cmd []string, _ api.AttachIO) error {
	s.execs.Add(1)
	name := ""
	if len(cmd) > 0 {
		name = cmd[0]
	}
	s.mu.Lock()
	c := s.count[name]
	if c == nil {
		c = &atomic.Int32{}
		s.count[name] = c
	}
	block := s.block[name]
	fail := s.fail[name]
	s.mu.Unlock()
	c.Add(1)
	if block {
		s.once.Do(func() { close(s.entered) })
		<-ctx.Done()
		return ctx.Err()
	}
	if fail != nil && fail.Load() {
		s.failures(name).Add(1)
		return fmt.Errorf("exit code 1")
	}
	return nil
}

func execProbe(cmd string, period, timeout, fail, success int32) *corev1.Probe {
	p := &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{cmd}}},
		PeriodSeconds:    period,
		TimeoutSeconds:   timeout,
		FailureThreshold: fail,
	}
	if success > 0 {
		p.SuccessThreshold = success
	}
	return p
}

// A failing sidecar liveness probe kills that container and the pod restart
// policy starts it again. The VM stays up and keeps its slot.
func TestSidecarLivenessExecRestartsContainer(t *testing.T) {
	rt := fake.New()
	sc := newCmdSidecar()
	sc.gate("live").Store(true)
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("side", "uid-live", corev1.RestartPolicyAlways)
	pod.Spec.Containers[1].LivenessProbe = execProbe("live", 1, 1, 1, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "side")
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("liveness failed the pod: %s", p.Status.Message)
		}
		side := (*corev1.ContainerStatus)(nil)
		if err == nil {
			side = containerStatus(p, "log")
		}
		if err == nil && p.Status.Phase == corev1.PodRunning && side != nil && side.RestartCount >= 1 && sc.creates.Load() >= 2 && sc.stops.Load() >= 1 {
			last := side.LastTerminationState.Terminated
			if last == nil || last.ExitCode != 137 {
				t.Fatalf("last exit %+v, want the kill code", last)
			}
			if e.slots.Used() != 1 || rt.Running() != 1 || rt.Starts() != 1 {
				t.Fatalf("slots=%d running=%d starts=%d, liveness restarted the VM or dropped the slot", e.slots.Used(), rt.Running(), rt.Starts())
			}
			if sc.calls("live").Load() == 0 {
				t.Fatal("liveness probe was not executed")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	p, _ := e.Get("default", "side")
	t.Fatalf("liveness probe was not executed or did not restart the container: creates=%d stops=%d execs=%d %+v", sc.creates.Load(), sc.stops.Load(), sc.execs.Load(), containerStatus(p, "log"))
}

func TestSidecarLivenessRespectsRestartPolicy(t *testing.T) {
	cases := []struct {
		name   string
		policy corev1.RestartPolicy
		fail   bool
	}{
		{name: "on-failure", policy: corev1.RestartPolicyOnFailure},
		{name: "never", policy: corev1.RestartPolicyNever, fail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := fake.New()
			sc := newCmdSidecar()
			sc.gate("live").Store(true)
			e := sidecarEngine(t, rt, sc)
			pod := hybridPod("side", "uid-live-"+tc.name, tc.policy)
			pod.Spec.Containers[1].LivenessProbe = execProbe("live", 1, 1, 2, 1)
			if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
				t.Fatal(err)
			}
			if tc.fail {
				p := waitPhase(t, e, "default", "side", corev1.PodFailed)
				if sc.creates.Load() != 1 {
					t.Fatalf("creates=%d, Never restarted the sidecar", sc.creates.Load())
				}
				if sc.stops.Load() < 1 {
					t.Fatalf("stops=%d, liveness failure did not stop the container", sc.stops.Load())
				}
				if !strings.Contains(p.Status.Message, "exited 137") {
					t.Fatalf("message %q, want the runtime exit code", p.Status.Message)
				}
				waitVMReaped(t, e, rt, "side", "uid-live-"+tc.name)
				return
			}
			deadline := time.Now().Add(4 * time.Second)
			for time.Now().Before(deadline) {
				p, err := e.Get("default", "side")
				if err == nil && p.Status.Phase == corev1.PodFailed {
					t.Fatalf("liveness failed the pod: %s", p.Status.Message)
				}
				side := (*corev1.ContainerStatus)(nil)
				if err == nil {
					side = containerStatus(p, "log")
				}
				if err == nil && side != nil && side.RestartCount >= 1 && sc.creates.Load() >= 2 {
					if e.slots.Used() != 1 || rt.Running() != 1 || rt.Starts() != 1 {
						t.Fatalf("slots=%d running=%d starts=%d", e.slots.Used(), rt.Running(), rt.Starts())
					}
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			p, _ := e.Get("default", "side")
			t.Fatalf("liveness probe was not executed or did not restart: creates=%d stops=%d %+v", sc.creates.Load(), sc.stops.Load(), containerStatus(p, "log"))
		})
	}
}

// The second liveness kill goes through the sidecar crash backoff. The
// container is not created again immediately, and the VM slot stays held.
func TestSidecarLivenessReusesCrashBackoff(t *testing.T) {
	rt := fake.New()
	sc := newCmdSidecar()
	sc.gate("live").Store(true)
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("side", "uid-live-bo", corev1.RestartPolicyAlways)
	pod.Spec.Containers[1].LivenessProbe = execProbe("live", 1, 1, 1, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "side")
		side := (*corev1.ContainerStatus)(nil)
		if err == nil {
			side = containerStatus(p, "log")
		}
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("liveness failed the pod: %s", p.Status.Message)
		}
		if err == nil && sc.creates.Load() == 2 && side != nil && side.State.Waiting != nil && side.State.Waiting.Reason == "CrashLoopBackOff" {
			n := sc.creates.Load()
			time.Sleep(200 * time.Millisecond)
			if got := sc.creates.Load(); got != n {
				t.Fatalf("creates %d -> %d during crash backoff", n, got)
			}
			if e.slots.Used() != 1 || rt.Running() != 1 || rt.Starts() != 1 {
				t.Fatalf("slots=%d running=%d starts=%d, backoff dropped the VM", e.slots.Used(), rt.Running(), rt.Starts())
			}
			if !strings.Contains(side.State.Waiting.Message, "log") {
				t.Fatalf("backoff message %q", side.State.Waiting.Message)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	p, _ := e.Get("default", "side")
	t.Fatalf("liveness probe was not executed or did not enter crash backoff: creates=%d stops=%d %+v", sc.creates.Load(), sc.stops.Load(), containerStatus(p, "log"))
}

func TestSidecarLivenessWithoutStopFailsPod(t *testing.T) {
	rt := fake.New()
	sc := newCmdSidecar()
	sc.gate("live").Store(true)
	sc.stopErr = fmt.Errorf("cannot stop")
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("side", "uid-live-nostop", corev1.RestartPolicyAlways)
	pod.Spec.Containers[1].LivenessProbe = execProbe("live", 1, 1, 1, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "side", corev1.PodFailed)
	if sc.execs.Load() == 0 {
		t.Fatal("liveness probe was not executed")
	}
	if sc.stops.Load() != 0 {
		t.Fatalf("stops=%d, the failed stop was counted as a kill", sc.stops.Load())
	}
	if !strings.Contains(p.Status.Message, "liveness") {
		t.Fatalf("message %q", p.Status.Message)
	}
	waitVMReaped(t, e, rt, "side", "uid-live-nostop")
}

// A failing readiness probe clears the container and the pod Ready
// conditions and does not kill the container or the VM.
func TestSidecarReadinessExecClearsReady(t *testing.T) {
	rt := fake.New()
	sc := newCmdSidecar()
	sc.gate("ready").Store(true)
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("side", "uid-ready", corev1.RestartPolicyAlways)
	pod.Spec.Containers[1].ReadinessProbe = execProbe("ready", 1, 1, 1, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "side")
		if err != nil || p.Status.Phase != corev1.PodRunning {
			if err == nil && p.Status.Phase == corev1.PodFailed {
				t.Fatalf("readiness failed the pod: %s", p.Status.Message)
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		side := containerStatus(p, "log")
		if side == nil || side.State.Running == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if sc.calls("ready").Load() == 0 || side.Ready || podReady(p) {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if !statusStarted(side) {
			t.Fatalf("readiness failure left Ready=%v podReady=%v started=%v %+v", side.Ready, podReady(p), statusStarted(side), side)
		}
		c, ok := cond(p, corev1.PodReady)
		if !ok || c.Status != corev1.ConditionFalse || c.Reason != "ReadinessProbe" || !strings.Contains(c.Message, "log") {
			t.Fatalf("PodReady %+v", c)
		}
		cr, ok := cond(p, corev1.ContainersReady)
		if !ok || cr.Status != corev1.ConditionFalse || cr.Reason != "ReadinessProbe" {
			t.Fatalf("ContainersReady %+v", cr)
		}
		if sc.stops.Load() != 0 || sc.creates.Load() != 1 || side.RestartCount != 0 {
			t.Fatalf("readiness restarted the container: creates=%d stops=%d %+v", sc.creates.Load(), sc.stops.Load(), side)
		}
		if e.slots.Used() != 1 || rt.Running() != 1 || rt.Starts() != 1 {
			t.Fatalf("slots=%d running=%d starts=%d", e.slots.Used(), rt.Running(), rt.Starts())
		}
		main := containerStatus(p, "macos")
		if main == nil || !main.Ready {
			t.Fatalf("VM container %+v", main)
		}
		sc.gate("ready").Store(false)
		deadline = time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			p, err = e.Get("default", "side")
			if err == nil && p.Status.Phase == corev1.PodRunning {
				side = containerStatus(p, "log")
				if side != nil && side.Ready && podReady(p) && side.State.Running != nil && sc.stops.Load() == 0 {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		p, _ = e.Get("default", "side")
		t.Fatalf("readiness did not recover: %+v podReady=%v", containerStatus(p, "log"), podReady(p))
	}
	p, _ := e.Get("default", "side")
	t.Fatalf("readiness probe was not executed and Ready stayed %v: execs=%d %+v", containerStatus(p, "log") != nil && containerStatus(p, "log").Ready, sc.execs.Load(), containerStatus(p, "log"))
}

func TestSidecarReadinessSuccessThreshold(t *testing.T) {
	rt := fake.New()
	sc := newCmdSidecar()
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("side", "uid-ready-ok", corev1.RestartPolicyAlways)
	pod.Spec.Containers[1].ReadinessProbe = execProbe("ready", 1, 1, 5, 3)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	saw := false
	for time.Now().Before(deadline) {
		if sc.calls("ready").Load() < 1 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		p, err := e.Get("default", "side")
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		side := containerStatus(p, "log")
		if side != nil && side.Ready && sc.calls("ready").Load() < 3 {
			t.Fatalf("ready after %d successes; successThreshold is 3", sc.calls("ready").Load())
		}
		if side != nil && !side.Ready && !podReady(p) {
			saw = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !saw {
		t.Fatalf("readiness probe was not executed or Ready flipped early: execs=%d", sc.calls("ready").Load())
	}
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "side")
		if err == nil {
			side := containerStatus(p, "log")
			if side != nil && side.Ready && podReady(p) && sc.calls("ready").Load() >= 3 && sc.stops.Load() == 0 && sc.creates.Load() == 1 {
				if e.slots.Used() != 1 || rt.Starts() != 1 {
					t.Fatalf("slots=%d starts=%d", e.slots.Used(), rt.Starts())
				}
				return
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ := e.Get("default", "side")
	t.Fatalf("readiness did not pass after successThreshold: execs=%d %+v", sc.calls("ready").Load(), containerStatus(p, "log"))
}

func TestSidecarReadinessFailureThreshold(t *testing.T) {
	rt := fake.New()
	sc := newCmdSidecar()
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("side", "uid-ready-fail", corev1.RestartPolicyAlways)
	pod.Spec.Containers[1].ReadinessProbe = execProbe("ready", 1, 1, 3, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var passed int32
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "side")
		if err == nil && p.Status.Phase == corev1.PodRunning {
			side := containerStatus(p, "log")
			if side != nil && side.Ready && podReady(p) && sc.calls("ready").Load() >= 1 {
				passed = sc.calls("ready").Load()
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if passed == 0 {
		t.Fatalf("readiness probe was not executed: execs=%d", sc.calls("ready").Load())
	}
	sc.gate("ready").Store(true)
	one := false
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		// Completed failures only. calls includes a probe that has started
		// but not yet read the gate, so calls-passed can show 2 when the
		// third failure has already landed.
		fails := sc.failures("ready").Load()
		p, err := e.Get("default", "side")
		if err != nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		side := containerStatus(p, "log")
		if side == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if sc.stops.Load() != 0 || sc.creates.Load() != 1 {
			t.Fatalf("readiness restarted the container: creates=%d stops=%d", sc.creates.Load(), sc.stops.Load())
		}
		if fails >= 1 && fails < 3 && (!side.Ready || !podReady(p)) {
			t.Fatalf("readiness flipped before failureThreshold: fails=%d %+v podReady=%v", fails, side, podReady(p))
		}
		if fails >= 1 && fails < 3 && side.Ready && podReady(p) {
			one = true
		}
		if fails >= 3 && !side.Ready && !podReady(p) {
			if !one {
				t.Fatal("did not observe Ready staying true below failureThreshold")
			}
			c, ok := cond(p, corev1.PodReady)
			if !ok || c.Reason != "ReadinessProbe" {
				t.Fatalf("PodReady %+v", c)
			}
			if e.slots.Used() != 1 || rt.Running() != 1 {
				t.Fatalf("slots=%d running=%d", e.slots.Used(), rt.Running())
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("readiness failureThreshold was not applied: execs=%d failures=%d passedAt=%d", sc.calls("ready").Load(), sc.failures("ready").Load(), passed)
}

func TestSidecarLivenessAndReadinessWaitForStartup(t *testing.T) {
	rt := fake.New()
	sc := newCmdSidecar()
	sc.gate("boot").Store(true)
	sc.gate("live").Store(true)
	sc.gate("ready").Store(true)
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("side", "uid-gate", corev1.RestartPolicyAlways)
	pod.Spec.Containers[1].StartupProbe = execProbe("boot", 1, 1, 100, 1)
	pod.Spec.Containers[1].LivenessProbe = execProbe("live", 1, 1, 1, 1)
	pod.Spec.Containers[1].ReadinessProbe = execProbe("ready", 1, 1, 1, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	held := false
	for time.Now().Before(deadline) {
		if sc.calls("boot").Load() == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if sc.calls("live").Load() != 0 || sc.calls("ready").Load() != 0 || sc.stops.Load() != 0 {
			t.Fatalf("liveness/readiness ran before startup passed: live=%d ready=%d stops=%d", sc.calls("live").Load(), sc.calls("ready").Load(), sc.stops.Load())
		}
		p, err := e.Get("default", "side")
		side := (*corev1.ContainerStatus)(nil)
		if err == nil {
			side = containerStatus(p, "log")
		}
		if side != nil && !statusStarted(side) && !side.Ready {
			held = true
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !held {
		t.Fatal("startup probe did not hold the sidecar")
	}
	sc.gate("boot").Store(false)
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if sc.calls("live").Load() > 0 && sc.stops.Load() > 0 && rt.Starts() == 1 && e.slots.Used() == 1 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("liveness did not run after startup passed: boot=%d live=%d ready=%d stops=%d", sc.calls("boot").Load(), sc.calls("live").Load(), sc.calls("ready").Load(), sc.stops.Load())
}

func TestSidecarLivenessInitialDelay(t *testing.T) {
	rt := fake.New()
	sc := newCmdSidecar()
	sc.gate("live").Store(true)
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("side", "uid-delay", corev1.RestartPolicyAlways)
	p := execProbe("live", 1, 1, 1, 1)
	p.InitialDelaySeconds = 1
	pod.Spec.Containers[1].LivenessProbe = p
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(350 * time.Millisecond)
	if n := sc.calls("live").Load(); n != 0 {
		t.Fatalf("liveness ran %d times during initialDelaySeconds", n)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sc.calls("live").Load() > 0 && sc.stops.Load() > 0 {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("liveness probe did not run after the initial delay: execs=%d stops=%d", sc.calls("live").Load(), sc.stops.Load())
}

func TestSidecarLivenessTimeoutCountsAsFailure(t *testing.T) {
	rt := fake.New()
	sc := newCmdSidecar()
	sc.block["live"] = true
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("side", "uid-timeout", corev1.RestartPolicyAlways)
	pod.Spec.Containers[1].LivenessProbe = execProbe("live", 30, 1, 1, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sc.entered:
	case <-time.After(800 * time.Millisecond):
		t.Fatal("liveness probe was not executed")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sc.stops.Load() > 0 {
			if rt.Starts() != 1 || e.slots.Used() != 1 {
				t.Fatalf("slots=%d starts=%d", e.slots.Used(), rt.Starts())
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out liveness probe was not a failure: stops=%d execs=%d", sc.stops.Load(), sc.execs.Load())
}

func TestSidecarReadinessHTTP(t *testing.T) {
	const secret = "READY-SECRET-BODY"
	var pass atomic.Bool
	var hits atomic.Int32
	var saw503 atomic.Bool
	rt := fake.New()
	e := sidecarEngine(t, rt, nil)
	ip, port := serveRoutable(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/ready" {
			http.NotFound(w, r)
			return
		}
		if !pass.Load() {
			saw503.Store(true)
			http.Error(w, secret, http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	e.sidecar = addressedRuntime{Runtime: sidecar.NewMemory(), ip: ip}
	pod := hybridPod("side", "uid-http-ready", corev1.RestartPolicyAlways)
	pod.Spec.Containers[1].ReadinessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Port: intstr.FromInt(port), Path: "/ready",
		}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 1,
		SuccessThreshold: 1,
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	held := false
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "side")
		if err != nil || p.Status.Phase != corev1.PodRunning {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		side := containerStatus(p, "log")
		if !saw503.Load() {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if side != nil && (side.Ready || podReady(p)) {
			t.Fatalf("HTTP 503 left the sidecar ready: %+v hits=%d", side, hits.Load())
		}
		assertNoProbeBody(t, p, secret)
		held = true
		break
	}
	if !held {
		t.Fatal("HTTP readiness probe was not executed and Ready stayed true")
	}
	pass.Store(true)
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "side")
		if err == nil {
			side := containerStatus(p, "log")
			if side != nil && side.Ready && podReady(p) && side.State.Running != nil {
				return
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("HTTP readiness did not recover: hits=%d", hits.Load())
}

func TestSidecarLivenessTCPRestarts(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}
	rt := fake.New()
	e := sidecarEngine(t, rt, sidecar.NewMemory())
	pod := hybridPod("side", "uid-tcp-live", corev1.RestartPolicyAlways)
	pod.Spec.Containers[1].LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
			Host: "127.0.0.1", Port: intstr.FromInt(port),
		}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 1,
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "side")
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("TCP liveness failed the pod: %s", p.Status.Message)
		}
		side := (*corev1.ContainerStatus)(nil)
		if err == nil {
			side = containerStatus(p, "log")
		}
		if err == nil && side != nil && side.RestartCount >= 1 {
			if e.slots.Used() != 1 || rt.Running() != 1 || rt.Starts() != 1 {
				t.Fatalf("slots=%d running=%d starts=%d", e.slots.Used(), rt.Running(), rt.Starts())
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ := e.Get("default", "side")
	t.Fatalf("TCP liveness probe was not executed or did not restart: %+v", containerStatus(p, "log"))
}

// manualClock is the test clock. Advance moves it and fires timers whose
// deadline has passed. The watch blocks on those timers instead of the wall.
type manualClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*manualTimer
}

type manualTimer struct {
	c       *manualClock
	ch      chan time.Time
	when    time.Time
	stopped bool
}

func newManualClock(now time.Time) *manualClock {
	return &manualClock{now: now}
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) NewTimer(d time.Duration) probeTimer {
	if d < 0 {
		d = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t := &manualTimer{c: c, ch: make(chan time.Time, 1), when: c.now.Add(d)}
	c.waiters = append(c.waiters, t)
	if d == 0 {
		t.stopped = true
		t.ch <- c.now
	}
	return t
}

func (t *manualTimer) C() <-chan time.Time { return t.ch }

func (t *manualTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if t.stopped {
		return false
	}
	t.stopped = true
	return true
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now
	var fire []chan time.Time
	keep := make([]*manualTimer, 0, len(c.waiters))
	for _, t := range c.waiters {
		if t.stopped {
			continue
		}
		if !t.when.After(now) {
			t.stopped = true
			fire = append(fire, t.ch)
			continue
		}
		keep = append(keep, t)
	}
	c.waiters = keep
	c.mu.Unlock()
	for _, ch := range fire {
		select {
		case ch <- now:
		default:
		}
	}
}

func (c *manualClock) WaitArmed(timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		armed := false
		for _, t := range c.waiters {
			if !t.stopped {
				armed = true
				break
			}
		}
		c.mu.Unlock()
		if armed {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

func TestProbePeriodFollowsTheInjectedClock(t *testing.T) {
	clk := newManualClock(time.Now())
	rt := fake.New()
	sc := &probeSidecar{}
	e := sidecarEngine(t, rt, sc)
	e.clock = clk
	e.cfg.ProbeInterval = time.Hour
	pod := sidecarStartupPod("clock", &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sidecar-boot"}}},
		PeriodSeconds:    5,
		TimeoutSeconds:   1,
		FailureThreshold: 1000,
	})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for sc.execs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sc.execs.Load() != 1 {
		t.Fatalf("first probe execs=%d", sc.execs.Load())
	}
	if !clk.WaitArmed(2 * time.Second) {
		t.Fatal("watch did not arm a timer on the injected clock")
	}
	time.Sleep(100 * time.Millisecond)
	if n := sc.execs.Load(); n != 1 {
		t.Fatalf("probe ran %d times on wall time; the watch interval woke it", n)
	}
	clk.Advance(5 * time.Second)
	deadline = time.Now().Add(2 * time.Second)
	for sc.execs.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := sc.execs.Load(); n != 2 {
		t.Fatalf("second probe did not run when the clock advanced by periodSeconds: execs=%d", n)
	}
}

func TestDeleteDuringSidecarLivenessDoesNotRestart(t *testing.T) {
	rt := fake.New()
	sc := newCmdSidecar()
	sc.block["live"] = true
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("side", "uid-live-del", corev1.RestartPolicyAlways)
	pod.Spec.Containers[1].LivenessProbe = execProbe("live", 30, 5, 1, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sc.entered:
	case <-time.After(800 * time.Millisecond):
		t.Fatal("liveness probe was not executed")
	}
	creates := sc.creates.Load()
	if err := e.Delete(context.Background(), "default", "side", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if sc.creates.Load() != creates || sc.stops.Load() != 0 {
		t.Fatalf("creates %d -> %d stops=%d; cancelling the liveness probe restarted the container", creates, sc.creates.Load(), sc.stops.Load())
	}
	if rt.Running() != 0 || e.slots.Used() != 0 || rt.Starts() != 1 {
		t.Fatalf("running=%d slots=%d starts=%d", rt.Running(), e.slots.Used(), rt.Starts())
	}
}

func TestSidecarStartupProbeHonorsPeriodSeconds(t *testing.T) {
	rt := fake.New()
	sc := &probeSidecar{}
	e := sidecarEngine(t, rt, sc)
	pod := sidecarStartupPod("period", &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sidecar-boot"}}},
		PeriodSeconds:    30,
		TimeoutSeconds:   1,
		FailureThreshold: 1000,
	})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for sc.execs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sc.execs.Load() == 0 {
		t.Fatal("startup probe did not run")
	}
	time.Sleep(250 * time.Millisecond)
	if n := sc.execs.Load(); n != 1 {
		t.Fatalf("startup probe ran %d times; PeriodSeconds is still the watch interval", n)
	}
}

func TestMainReadinessProbeHonorsPeriodSeconds(t *testing.T) {
	rt := fake.New()
	var n atomic.Int32
	rt.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "ready" {
			n.Add(1)
			return 1, nil
		}
		return 0, nil
	}
	e := sidecarEngine(t, rt, sidecar.None{})
	pod := samplePod("rd", "uid-period-rd")
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"ready"}}},
		PeriodSeconds:    30,
		TimeoutSeconds:   1,
		FailureThreshold: 3,
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for n.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n.Load() == 0 {
		t.Fatal("readiness probe did not run")
	}
	time.Sleep(250 * time.Millisecond)
	if got := n.Load(); got != 1 {
		t.Fatalf("readiness probe ran %d times; PeriodSeconds is still the watch interval", got)
	}
}

func TestUnsetProbePeriodDefaultsToTenSeconds(t *testing.T) {
	rt := fake.New()
	sc := &probeSidecar{}
	e := sidecarEngine(t, rt, sc)
	pod := sidecarStartupPod("deflt", &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sidecar-boot"}}},
		TimeoutSeconds:   1,
		FailureThreshold: 1000,
	})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for sc.execs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sc.execs.Load() == 0 {
		t.Fatal("startup probe did not run")
	}
	time.Sleep(250 * time.Millisecond)
	if n := sc.execs.Load(); n != 1 {
		t.Fatalf("unset PeriodSeconds ran %d times; the default is still the watch interval", n)
	}
}
