package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

func drainEngine(t *testing.T, cfg config.Config, sc sidecar.Runtime) (*Engine, *fake.Runtime) {
	t.Helper()
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	rt := fake.New()
	e := New(cfg, slots, rt, sc, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)
	return e, rt
}

func drainCfg(t *testing.T) config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	return cfg
}

func TestDrainDeletesAllPodsAndSnapshotsCaches(t *testing.T) {
	cfg := drainCfg(t)
	e, rt := drainEngine(t, cfg, sidecar.None{})

	pod := cachePod("d1", "uid-d1", map[string]string{"cache.darwin.node/xc": "/Users/mac/DD"})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	if err := e.Create(context.Background(), samplePod("d2", "uid-d2"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "d1", corev1.PodRunning)
	waitPhase(t, e, "default", "d2", corev1.PodRunning)

	// Simulated guest write flows through the virtio-fs share to host disk.
	hostDir := e.cachePodDir("uid-d1", "xc")
	if err := os.WriteFile(filepath.Join(hostDir, "state.bin"), []byte("drain-me"), 0o644); err != nil {
		t.Fatal(err)
	}

	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Drain(dctx); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"d1", "d2"} {
		if _, err := e.Get("default", name); err == nil {
			t.Fatalf("pod %s survived drain", name)
		}
	}
	if used := e.slots.Used(); used != 0 {
		t.Fatalf("slots not freed: %d", used)
	}
	b, err := os.ReadFile(filepath.Join(e.cacheStorePath("default", "xc"), "state.bin"))
	if err != nil || string(b) != "drain-me" {
		t.Fatalf("cache snapshot lost across drain: %q %v", b, err)
	}
	if rt.Starts() < 2 || rt.Starts() > 2+1 {
		t.Fatalf("unexpected boot count %d (warm replenisher may have run during drain)", rt.Starts())
	}
}

func TestDrainRejectsNewCreates(t *testing.T) {
	cfg := drainCfg(t)
	e, _ := drainEngine(t, cfg, sidecar.None{})
	if err := e.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !e.Draining() {
		t.Fatal("Draining() must be true after Drain")
	}
	err := e.Create(context.Background(), samplePod("late", "uid-late"), Credentials{})
	if err == nil {
		t.Fatal("create during drain must fail")
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, "pods", "uid-late")); !os.IsNotExist(err) {
		t.Fatalf("rejected pod must leave no state: %v", err)
	}
}

// budgetSidecar blocks the in-flight delete until the drain context is
// canceled. Close and a later delete pass a live context; unblock lets
// those return without a timer.
type budgetSidecar struct {
	sidecar.None
	once    sync.Once
	entered chan struct{}
	unblock chan struct{}
}

func (s *budgetSidecar) RemovePod(ctx context.Context, _, _ string, _ int64) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-ctx.Done():
	case <-s.unblock:
	}
	return nil
}

func TestDrainHonorsContextBudget(t *testing.T) {
	cfg := drainCfg(t)
	slots, _ := capacity.New(2)
	rt := fake.New()
	unblock := make(chan struct{})
	sc := &budgetSidecar{entered: make(chan struct{}), unblock: unblock}
	e := New(cfg, slots, rt, sc, event.Nop{}, "10.0.0.1")
	// Close force-stops whatever the budget left behind, and that path calls
	// RemovePod with a context that is not the drain context.
	t.Cleanup(e.Close)
	t.Cleanup(func() { close(unblock) })

	pods := []struct{ name, uid string }{{"pa", "uid-a"}, {"pb", "uid-b"}}
	for _, p := range pods {
		if err := e.Create(context.Background(), samplePod(p.name, p.uid), Credentials{}); err != nil {
			t.Fatal(err)
		}
	}
	waitPhase(t, e, "default", "pa", corev1.PodRunning)
	waitPhase(t, e, "default", "pb", corev1.PodRunning)

	dctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-sc.entered
		cancel()
	}()
	if err := e.Drain(dctx); err != nil {
		t.Fatal(err)
	}
	// pa sorts first. Its delete is already inside RemovePod when the
	// context ends, so it finishes. pb must not be started.
	if _, err := e.Get("default", "pa"); err == nil {
		t.Fatal("in-flight delete of pa did not finish")
	}
	if _, err := e.Get("default", "pb"); err != nil {
		t.Fatal("drain started pb after its context ended")
	}
}

func TestWarmReplenisherPausedWhileDraining(t *testing.T) {
	cfg := drainCfg(t)
	cfg.WarmSlots = 1
	cfg.WarmImage = warmImg
	e, _ := drainEngine(t, cfg, sidecar.None{})

	if err := e.Create(context.Background(), samplePod("busy", "uid-busy"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "busy", corev1.PodRunning)

	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e.Drain(dctx); err != nil {
		t.Fatal(err)
	}
	// The freed slot must stay free: no fresh warm boots after drain.
	deadline := time.Now().Add(1600 * time.Millisecond)
	for time.Now().Before(deadline) {
		if n := e.warm.len(); n != 0 {
			t.Fatalf("warm pool rebooted during/after drain: %d entries", n)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
