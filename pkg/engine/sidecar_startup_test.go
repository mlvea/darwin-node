package engine

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// probeSidecar is a running host container whose exec startup probe the test
// can hold closed. Create leaves it running; it does not exit.
type probeSidecar struct {
	sidecar.None
	mu      sync.Mutex
	st      sidecar.Status
	pass    atomic.Bool
	execs   atomic.Int32
	creates atomic.Int32
	stops   atomic.Int32
	stopErr error
}

func (s *probeSidecar) Create(_ context.Context, _ *corev1.Pod, c corev1.Container, _ string) error {
	s.creates.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st = sidecar.Status{Name: c.Name, ID: "mem://" + c.Name, State: "running", StartedAt: time.Now()}
	return nil
}

// StopContainer is what a startup probe past its threshold has to call.
// The container stays in the pod and reports the kill's exit code.
func (s *probeSidecar) StopContainer(context.Context, string, string, string) error {
	if s.stopErr != nil {
		return s.stopErr
	}
	s.stops.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.State = "terminated"
	s.st.ExitCode = 137
	s.st.FinishedAt = time.Now()
	return nil
}

func (s *probeSidecar) List(context.Context, string, string) ([]sidecar.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Name == "" {
		return nil, nil
	}
	return []sidecar.Status{s.st}, nil
}

func (s *probeSidecar) kill() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.State = "terminated"
	s.st.ExitCode = 1
	s.st.FinishedAt = time.Now()
}

func (s *probeSidecar) Exec(_ context.Context, _, _, _ string, cmd []string, _ api.AttachIO) error {
	s.execs.Add(1)
	if len(cmd) > 0 && cmd[0] == "sidecar-boot" && !s.pass.Load() {
		return fmt.Errorf("exit code 1")
	}
	return nil
}

func sidecarStartupPod(name string, probe *corev1.Probe) *corev1.Pod {
	pod := hybridPod(name, "uid-"+name, corev1.RestartPolicyAlways)
	pod.Spec.Containers[1].StartupProbe = probe
	return pod
}

func TestSidecarStartupProbeGatesStarted(t *testing.T) {
	rt := fake.New()
	sc := &probeSidecar{}
	e := sidecarEngine(t, rt, sc)
	pod := sidecarStartupPod("side", &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sidecar-boot"}}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 1000,
	})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "side", corev1.PodRunning)

	deadline := time.Now().Add(5 * time.Second)
	held := false
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "side")
		if err != nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if p.Status.Phase == corev1.PodFailed {
			t.Fatalf("startup probe failed the pod: %s", p.Status.Message)
		}
		side := containerStatus(p, "log")
		main := containerStatus(p, "macos")
		if side == nil || side.State.Running == nil || main == nil || !main.Ready {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if sc.execs.Load() == 0 {
			if statusStarted(side) {
				t.Fatal("sidecar startup probe was not executed and Started was already true")
			}
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if statusStarted(side) || side.Ready || podReady(p) {
			t.Fatalf("sidecar startup probe failed but the container is started: %+v podReady=%v execs=%d", side, podReady(p), sc.execs.Load())
		}
		held = true
		break
	}
	if !held {
		p, _ := e.Get("default", "side")
		t.Fatalf("did not observe a running sidecar held not-started by its startup probe: %+v execs=%d", containerStatus(p, "log"), sc.execs.Load())
	}
	if e.slots.Used() != 1 || rt.Running() != 1 {
		t.Fatalf("slots=%d running=%d, a not-yet-started sidecar must keep the VM", e.slots.Used(), rt.Running())
	}

	sc.pass.Store(true)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "side")
		if err != nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if p.Status.Phase == corev1.PodFailed {
			t.Fatalf("pod failed after the sidecar startup probe passed: %s", p.Status.Message)
		}
		side := containerStatus(p, "log")
		if side == nil || !statusStarted(side) || !side.Ready || side.State.Running == nil || !podReady(p) {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		main := containerStatus(p, "macos")
		if main == nil || !statusStarted(main) || !main.Ready || main.RestartCount != 0 {
			t.Fatalf("VM container %+v", main)
		}
		if e.slots.Used() != 1 || rt.Running() != 1 {
			t.Fatalf("slots=%d running=%d", e.slots.Used(), rt.Running())
		}
		return
	}
	p, _ := e.Get("default", "side")
	t.Fatalf("sidecar startup probe passed but Started stayed false: %+v execs=%d podReady=%v", containerStatus(p, "log"), sc.execs.Load(), podReady(p))
}

func TestSidecarStartupProbeRerunsAfterRestart(t *testing.T) {
	rt := fake.New()
	sc := &probeSidecar{}
	sc.pass.Store(true)
	e := sidecarEngine(t, rt, sc)
	pod := sidecarStartupPod("again", &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sidecar-boot"}}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 1000,
	})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "again")
		if err == nil && p.Status.Phase == corev1.PodRunning {
			side := containerStatus(p, "log")
			if side != nil && statusStarted(side) && side.Ready && podReady(p) {
				break
			}
		}
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("startup failed the pod: %s", p.Status.Message)
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, err := e.Get("default", "again")
	if err != nil {
		t.Fatal(err)
	}
	if side := containerStatus(p, "log"); side == nil || !statusStarted(side) {
		t.Fatalf("sidecar did not start: %+v", side)
	}

	execs := sc.execs.Load()
	sc.pass.Store(false)
	sc.kill()
	deadline = time.Now().Add(5 * time.Second)
	saw := false
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "again")
		if err != nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if p.Status.Phase == corev1.PodFailed {
			t.Fatalf("restart failed the pod: %s", p.Status.Message)
		}
		side := containerStatus(p, "log")
		if side == nil || side.RestartCount < 1 || side.State.Running == nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if statusStarted(side) || side.Ready || podReady(p) {
			if sc.execs.Load() > execs {
				t.Fatalf("restarted sidecar stayed started before its startup probe passed: %+v execs=%d", side, sc.execs.Load())
			}
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if sc.execs.Load() <= execs {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		saw = true
		break
	}
	if !saw {
		p, _ := e.Get("default", "again")
		t.Fatalf("startup probe did not run again after the sidecar restart: %+v execs=%d", containerStatus(p, "log"), sc.execs.Load())
	}

	sc.pass.Store(true)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "again")
		if err != nil || p.Status.Phase != corev1.PodRunning {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		side := containerStatus(p, "log")
		if side != nil && statusStarted(side) && side.Ready && podReady(p) && side.RestartCount >= 1 {
			if e.slots.Used() != 1 || rt.Running() != 1 {
				t.Fatalf("slots=%d running=%d", e.slots.Used(), rt.Running())
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ = e.Get("default", "again")
	t.Fatalf("startup probe did not pass after the sidecar restart: %+v", containerStatus(p, "log"))
}

// A startup probe that exhausts its threshold kills the sidecar. The pod
// restart policy then decides. Always and OnFailure start it again and the
// pod stays Running with its VM slot. Never does not start it, and the
// non-zero kill fails the pod the same way any other non-zero sidecar exit
// does. Restartable init containers in kubelet are restarted from the
// container policy (Always); hybrid sidecars here follow the pod policy,
// which is the policy the rest of the sidecar restart path already uses.
func TestSidecarStartupProbeFailureFollowsRestartPolicy(t *testing.T) {
	cases := []struct {
		name   string
		policy corev1.RestartPolicy
		fail   bool
	}{
		{name: "always", policy: corev1.RestartPolicyAlways},
		{name: "on-failure", policy: corev1.RestartPolicyOnFailure},
		{name: "never", policy: corev1.RestartPolicyNever, fail: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := fake.New()
			sc := &probeSidecar{}
			e := sidecarEngine(t, rt, sc)
			pod := hybridPod("side", "uid-side-"+tc.name, tc.policy)
			pod.Spec.Containers[1].StartupProbe = &corev1.Probe{
				ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sidecar-boot"}}},
				PeriodSeconds:    1,
				TimeoutSeconds:   1,
				FailureThreshold: 2,
			}
			if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
				t.Fatal(err)
			}
			if tc.fail {
				p := waitPhase(t, e, "default", "side", corev1.PodFailed)
				if sc.creates.Load() != 1 {
					t.Fatalf("creates=%d, Never restarted the sidecar", sc.creates.Load())
				}
				if sc.stops.Load() < 1 {
					t.Fatalf("stops=%d, startup failure did not stop the container", sc.stops.Load())
				}
				if !strings.Contains(p.Status.Message, "exited 137") {
					t.Fatalf("message %q, want the runtime exit code", p.Status.Message)
				}
				waitVMReaped(t, e, rt, "side", "uid-side-"+tc.name)
				return
			}
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				p, err := e.Get("default", "side")
				if err == nil && p.Status.Phase == corev1.PodFailed {
					t.Fatalf("startup probe failed the pod: %s", p.Status.Message)
				}
				side := (*corev1.ContainerStatus)(nil)
				if err == nil {
					side = containerStatus(p, "log")
				}
				if err == nil && p.Status.Phase == corev1.PodRunning && side != nil && side.RestartCount >= 1 {
					last := side.LastTerminationState.Terminated
					if last == nil || last.ExitCode != 137 {
						t.Fatalf("last exit %+v, want the kill code", last)
					}
					if e.slots.Used() != 1 || rt.Running() != 1 {
						t.Fatalf("slots=%d running=%d", e.slots.Used(), rt.Running())
					}
					if sc.creates.Load() < 2 || sc.stops.Load() < 1 || sc.execs.Load() < 2 {
						t.Fatalf("creates=%d stops=%d execs=%d", sc.creates.Load(), sc.stops.Load(), sc.execs.Load())
					}
					return
				}
				time.Sleep(15 * time.Millisecond)
			}
			p, _ := e.Get("default", "side")
			t.Fatalf("sidecar was not restarted after the startup probe failed: phase=%s creates=%d stops=%d %+v", p.Status.Phase, sc.creates.Load(), sc.stops.Load(), containerStatus(p, "log"))
		})
	}
}

// A runtime that cannot stop the container still fails the pod. There is
// no restart to apply until the container has actually exited.
func TestSidecarStartupProbeFailureWithoutStopFailsPod(t *testing.T) {
	rt := fake.New()
	sc := &probeSidecar{stopErr: fmt.Errorf("cannot stop")}
	e := sidecarEngine(t, rt, sc)
	pod := sidecarStartupPod("side", &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sidecar-boot"}}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 2,
	})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "side", corev1.PodFailed)
	if sc.execs.Load() < 2 {
		t.Fatalf("execs=%d, startup probe was not run to its failure threshold", sc.execs.Load())
	}
	if sc.stops.Load() != 0 {
		t.Fatalf("stops=%d, the failed stop was counted as a kill", sc.stops.Load())
	}
	if !strings.Contains(p.Status.Message, "startup probe failed") {
		t.Fatalf("message %q", p.Status.Message)
	}
	waitVMReaped(t, e, rt, "side", "uid-side")
}

func TestSidecarHTTPStartupProbeHitsTheHost(t *testing.T) {
	var pass atomic.Bool
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		if !pass.Load() {
			http.Error(w, "early", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	rt := fake.New()
	e := sidecarEngine(t, rt, sidecar.NewMemory())
	pod := sidecarStartupPod("http", &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Host: host, Port: intstr.FromInt(port), Path: "/healthz",
		}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 1000,
	})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "http", corev1.PodRunning)

	deadline := time.Now().Add(5 * time.Second)
	held := false
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "http")
		if err != nil || p.Status.Phase != corev1.PodRunning {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		side := containerStatus(p, "log")
		if side == nil || side.State.Running == nil {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if hits.Load() == 0 {
			if statusStarted(side) {
				t.Fatal("HTTP startup probe was not executed and Started was already true")
			}
			time.Sleep(15 * time.Millisecond)
			continue
		}
		if statusStarted(side) || podReady(p) {
			t.Fatalf("HTTP startup probe returned 503 but the sidecar is started: %+v hits=%d", side, hits.Load())
		}
		held = true
		break
	}
	if !held {
		p, _ := e.Get("default", "http")
		t.Fatalf("HTTP startup probe did not hold the sidecar: %+v hits=%d", containerStatus(p, "log"), hits.Load())
	}

	pass.Store(true)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "http")
		if err != nil || p.Status.Phase != corev1.PodRunning {
			if err == nil && p.Status.Phase == corev1.PodFailed {
				t.Fatalf("pod failed after HTTP startup passed: %s", p.Status.Message)
			}
			time.Sleep(15 * time.Millisecond)
			continue
		}
		side := containerStatus(p, "log")
		if side != nil && statusStarted(side) && side.Ready && podReady(p) {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ := e.Get("default", "http")
	t.Fatalf("HTTP startup probe passed but Started stayed false: %+v hits=%d", containerStatus(p, "log"), hits.Load())
}

func TestSidecarTCPStartupProbeDialsTheHost(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepts atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepts.Add(1)
			_ = c.Close()
		}
	}()
	_, portStr, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatal(err)
	}

	rt := fake.New()
	e := sidecarEngine(t, rt, sidecar.NewMemory())
	pod := sidecarStartupPod("tcp", &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{TCPSocket: &corev1.TCPSocketAction{
			Host: "127.0.0.1", Port: intstr.FromInt(port),
		}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 1000,
	})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "tcp", corev1.PodRunning)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "tcp")
		if err != nil || p.Status.Phase != corev1.PodRunning {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		side := containerStatus(p, "log")
		if side != nil && statusStarted(side) && accepts.Load() == 0 {
			t.Fatal("TCP startup probe was not executed and Started was already true")
		}
		if side != nil && statusStarted(side) && side.Ready && podReady(p) && accepts.Load() > 0 {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ := e.Get("default", "tcp")
	t.Fatalf("TCP startup probe did not pass: %+v accepts=%d", containerStatus(p, "log"), accepts.Load())
}
