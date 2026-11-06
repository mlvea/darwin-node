package main

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

func TestGracefulDrainUsesPodTerminationGrace(t *testing.T) {
	leakcheck.Check(t)
	sc := &graceSidecar{}
	eng, _ := newSignalEngine(t, sc)
	grace := int64(9)
	pod := signalPod("a", "uid-signal-a")
	pod.Spec.TerminationGracePeriodSeconds = &grace
	if err := eng.Create(context.Background(), pod, engine.Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSignalRunning(t, eng, "a")

	gate := newShutdownGate()
	gate.bind(eng)
	gate.Graceful(2 * time.Second)

	sc.mu.Lock()
	got := append([]int64(nil), sc.graces...)
	sc.mu.Unlock()
	if len(got) != 1 || got[0] != 9 {
		t.Fatalf("RemovePod graces %v, want [9] (drain before Close)", got)
	}
	if rt := eng.Slots().Used(); rt != 0 {
		t.Fatalf("slots still held: %d", rt)
	}
}

func TestSecondSignalClosesWithoutDrain(t *testing.T) {
	leakcheck.Check(t)
	eng, rt := newSignalEngine(t, sidecar.None{})
	for _, p := range []struct{ name, uid string }{{"a", "uid-sig-a"}, {"b", "uid-sig-b"}} {
		if err := eng.Create(context.Background(), signalPod(p.name, p.uid), engine.Credentials{}); err != nil {
			t.Fatal(err)
		}
		waitSignalRunning(t, eng, p.name)
	}
	if rt.Running() != 2 {
		t.Fatalf("running before signal: %d", rt.Running())
	}

	gate := newShutdownGate()
	gate.bind(eng)
	t.Cleanup(gate.Release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	go watchSecondSignal(ctx, func() <-chan os.Signal { return sig }, func(code int) {
		if got := rt.Running(); got != 0 {
			t.Errorf("second signal exited with %d VM(s) still running", got)
		}
		if got := eng.Slots().Used(); got != 0 {
			t.Errorf("second signal exited with %d slot(s) held", got)
		}
		exited <- code
		gate.Release()
	}, gate.Force)

	cancel()
	select {
	case code := <-exited:
		t.Fatalf("first signal exited %d", code)
	case <-time.After(50 * time.Millisecond):
	}
	if got := rt.Running(); got != 2 {
		t.Fatalf("first signal stopped VMs: %d running", got)
	}
	sig <- os.Interrupt
	select {
	case code := <-exited:
		if code != 130 {
			t.Fatalf("exit code %d", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("second signal did not exit")
	}
}

func TestGracefulZeroSkipsDrainAndStillCloses(t *testing.T) {
	leakcheck.Check(t)
	sc := &graceSidecar{}
	eng, rt := newSignalEngine(t, sc)
	grace := int64(9)
	pod := signalPod("a", "uid-signal-zero")
	pod.Spec.TerminationGracePeriodSeconds = &grace
	if err := eng.Create(context.Background(), pod, engine.Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSignalRunning(t, eng, "a")

	gate := newShutdownGate()
	gate.bind(eng)
	gate.Graceful(0)

	sc.mu.Lock()
	got := append([]int64(nil), sc.graces...)
	sc.mu.Unlock()
	if len(got) != 1 || got[0] != 0 {
		t.Fatalf("RemovePod graces %v, want [0] (Close, no drain)", got)
	}
	if rt.Running() != 0 || eng.Slots().Used() != 0 {
		t.Fatalf("running %d slots %d", rt.Running(), eng.Slots().Used())
	}
}

func TestSecondSignalDuringDrainStillCloses(t *testing.T) {
	leakcheck.Check(t)
	sc := newBlockSidecar()
	t.Cleanup(sc.release)
	eng, rt := newSignalEngine(t, sc)
	for _, p := range []struct{ name, uid string }{{"a", "uid-drain-a"}, {"b", "uid-drain-b"}} {
		pod := signalPod(p.name, p.uid)
		grace := int64(30)
		pod.Spec.TerminationGracePeriodSeconds = &grace
		if err := eng.Create(context.Background(), pod, engine.Credentials{}); err != nil {
			t.Fatal(err)
		}
		waitSignalRunning(t, eng, p.name)
	}

	gate := newShutdownGate()
	gate.bind(eng)
	t.Cleanup(gate.Release)
	graceDone := make(chan struct{})
	go func() {
		gate.Graceful(30 * time.Second)
		close(graceDone)
	}()
	select {
	case <-sc.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("drain did not reach RemovePod")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sig := make(chan os.Signal, 1)
	exited := make(chan int, 1)
	go watchSecondSignal(ctx, func() <-chan os.Signal { return sig }, func(code int) {
		if got := rt.Running(); got != 0 {
			t.Errorf("forced exit left %d VM(s) running", got)
		}
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
		t.Fatal("second signal did not finish Close before exit")
	}
	select {
	case <-graceDone:
	case <-time.After(3 * time.Second):
		t.Fatal("graceful path still running after forced Close")
	}
	sc.mu.Lock()
	maxIn := sc.maxIn
	sc.mu.Unlock()
	if maxIn != 1 {
		t.Fatalf("RemovePod overlapped %d times; drain and Close ran together", maxIn)
	}
	if _, err := eng.Get("default", "b"); err == nil {
		t.Fatal("pod b still registered")
	}
}

func newSignalEngine(t *testing.T, sc sidecar.Runtime) (*engine.Engine, *fake.Runtime) {
	t.Helper()
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = types.RuntimeFake
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	rt := fake.New()
	eng := engine.New(cfg, slots, rt, sc, event.Nop{}, "10.0.0.1")
	t.Cleanup(eng.Close)
	return eng, rt
}

func signalPod(name, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: k8stypes.UID(uid)},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "local/macos:test"}}},
	}
}

func waitSignalRunning(t *testing.T, eng *engine.Engine, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var p *corev1.Pod
	for time.Now().Before(deadline) {
		got, err := eng.Get("default", name)
		if err == nil && got.Status.Phase == corev1.PodRunning {
			return
		}
		p = got
		time.Sleep(15 * time.Millisecond)
	}
	if p == nil {
		t.Fatalf("%s not running", name)
	}
	t.Fatalf("%s not running: phase=%s msg=%s", name, p.Status.Phase, p.Status.Message)
}

type graceSidecar struct {
	sidecar.None
	mu     sync.Mutex
	graces []int64
}

func (s *graceSidecar) RemovePod(_ context.Context, _, _ string, grace int64) error {
	s.mu.Lock()
	s.graces = append(s.graces, grace)
	s.mu.Unlock()
	return nil
}

type blockSidecar struct {
	sidecar.None
	once    sync.Once
	entered chan struct{}
	unblock chan struct{}
	relOnce sync.Once
	mu      sync.Mutex
	in      int
	maxIn   int
}

func newBlockSidecar() *blockSidecar {
	return &blockSidecar{
		entered: make(chan struct{}),
		unblock: make(chan struct{}),
	}
}

func (s *blockSidecar) release() {
	s.relOnce.Do(func() { close(s.unblock) })
}

func (s *blockSidecar) RemovePod(ctx context.Context, _, _ string, grace int64) error {
	s.mu.Lock()
	s.in++
	if s.in > s.maxIn {
		s.maxIn = s.in
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.in--
		s.mu.Unlock()
	}()
	block := false
	s.once.Do(func() {
		block = true
		close(s.entered)
	})
	if block && grace > 0 {
		select {
		case <-ctx.Done():
		case <-s.unblock:
		}
	}
	return nil
}
