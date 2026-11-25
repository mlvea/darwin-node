package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// The first signal drains and then Close's. The second signal Close's
// without waiting out the drain. Both have to finish in-flight sidecar
// restarts and liveness restarts before they return: vz.Start ignores
// cancellation, and a Create already inside the watch can leave a
// container after RemovePod.

const signalLateDelay = 350 * time.Millisecond

type signalLateSidecar struct {
	sidecar.None
	mu      sync.Mutex
	byName  map[string]sidecar.Status
	creates int
	entered chan struct{}
	once    sync.Once
}

func newSignalLateSidecar() *signalLateSidecar {
	return &signalLateSidecar{byName: map[string]sidecar.Status{}, entered: make(chan struct{})}
}

func (s *signalLateSidecar) Create(_ context.Context, _ *corev1.Pod, c corev1.Container, _ string) error {
	s.mu.Lock()
	s.creates++
	n := s.creates
	s.mu.Unlock()
	if n >= 2 {
		s.once.Do(func() { close(s.entered) })
		time.Sleep(signalLateDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := sidecar.Status{Name: c.Name, ID: fmt.Sprintf("mem://%s-%d", c.Name, n), StartedAt: time.Now()}
	if n == 1 {
		st.State = "terminated"
		st.ExitCode = 1
		st.FinishedAt = st.StartedAt
	} else {
		st.State = "running"
	}
	s.byName[c.Name] = st
	return nil
}

func (s *signalLateSidecar) RemovePod(context.Context, string, string, int64) error {
	s.mu.Lock()
	s.byName = map[string]sidecar.Status{}
	s.mu.Unlock()
	return nil
}

func (s *signalLateSidecar) List(context.Context, string, string) ([]sidecar.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sidecar.Status, 0, len(s.byName))
	for _, st := range s.byName {
		out = append(out, st)
	}
	return out, nil
}

func (s *signalLateSidecar) running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, st := range s.byName {
		if st.State == "running" {
			n++
		}
	}
	return n
}

type signalLateRuntime struct {
	*fake.Runtime
	entered chan struct{}
	once    sync.Once
}

func (r *signalLateRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &signalLateMachine{Machine: m, rt: r}, nil
}

type signalLateMachine struct {
	runtime.Machine
	rt     *signalLateRuntime
	starts atomic.Int32
}

func (m *signalLateMachine) Start(ctx context.Context) error {
	if m.starts.Add(1) >= 2 {
		m.rt.once.Do(func() { close(m.rt.entered) })
		time.Sleep(signalLateDelay)
		return m.Machine.Start(context.Background())
	}
	return m.Machine.Start(ctx)
}

func signalHybrid(name, uid string) *corev1.Pod {
	p := signalPod(name, uid)
	p.Spec.RestartPolicy = corev1.RestartPolicyAlways
	p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "log", Image: "busybox"})
	return p
}

func signalLivenessPod(name, uid string) *corev1.Pod {
	p := signalPod(name, uid)
	p.Spec.RestartPolicy = corev1.RestartPolicyAlways
	p.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"live"}}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 1,
	}
	return p
}

func newSignalEng(t *testing.T, rt runtime.Runtime, sc sidecar.Runtime) *engine.Engine {
	t.Helper()
	leakcheck.Check(t)
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = types.RuntimeFake
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	cfg.ProbeInterval = 20 * time.Millisecond
	eng := engine.New(cfg, slots, rt, sc, event.Nop{}, "10.0.0.1")
	t.Cleanup(func() { _ = eng.Close() })
	return eng
}

func waitSignalEntered(t *testing.T, entered <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s did not start", what)
	}
}

func TestGracefulDuringSidecarRestartRemovesTheContainer(t *testing.T) {
	sc := newSignalLateSidecar()
	eng := newSignalEng(t, fake.New(), sc)
	if err := eng.Create(context.Background(), signalHybrid("a", "uid-sig-hy"), engine.Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSignalEntered(t, sc.entered, "sidecar restart")

	gate := newShutdownGate()
	gate.bind(eng)
	gate.Graceful(2 * time.Second)
	time.Sleep(signalLateDelay + 250*time.Millisecond)

	if n := sc.running(); n != 0 {
		t.Fatalf("first signal left %d sidecar container(s) running", n)
	}
	if eng.Slots().Used() != 0 {
		t.Fatalf("slots %d", eng.Slots().Used())
	}
}

func TestSecondSignalDuringLivenessRestartStopsTheVM(t *testing.T) {
	entered := make(chan struct{})
	inner := fake.New()
	inner.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "live" {
			return 1, nil
		}
		return 0, nil
	}
	rt := &signalLateRuntime{Runtime: inner, entered: entered}
	eng := newSignalEng(t, rt, sidecar.None{})
	if err := eng.Create(context.Background(), signalLivenessPod("a", "uid-sig-lv"), engine.Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSignalEntered(t, entered, "liveness restart")

	gate := newShutdownGate()
	gate.bind(eng)
	t.Cleanup(gate.Release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	go watchSecondSignal(ctx, func() <-chan os.Signal { return sig }, func(code int) {
		exited <- code
		gate.Release()
	}, gate.Force)

	cancel()
	sig <- os.Interrupt
	select {
	case code := <-exited:
		if code != 130 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second signal did not finish Close")
	}
	time.Sleep(signalLateDelay + 250*time.Millisecond)
	if got, slots := rt.Running(), eng.Slots().Used(); got != 0 || slots != 0 {
		t.Fatalf("second signal left running=%d slots=%d", got, slots)
	}
}
