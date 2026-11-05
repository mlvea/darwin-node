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
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

func closeTestEngine(t *testing.T) (*Engine, *fake.Runtime) {
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
	rt := fake.New()
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)
	return e, rt
}

func TestCloseStopsRunningPodVMs(t *testing.T) {
	e, rt := closeTestEngine(t)
	ctx := context.Background()
	if err := e.Create(ctx, samplePod("a", "uid-close-a"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	if err := e.Create(ctx, samplePod("b", "uid-close-b"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "a", corev1.PodRunning)
	waitPhase(t, e, "default", "b", corev1.PodRunning)
	if got := rt.Running(); got != 2 {
		t.Fatalf("running VMs before close: %d", got)
	}

	e.Close()

	if got := rt.Running(); got != 0 {
		t.Fatalf("Close left %d VM(s) running", got)
	}
	if got := e.slots.Used(); got != 0 {
		t.Fatalf("Close left %d slot(s) held", got)
	}
	if _, err := e.Get("default", "a"); err == nil {
		t.Fatal("pod a still registered after Close")
	}
	if _, err := os.Stat(filepath.Join(e.cfg.CacheDir, "pods", "uid-close-a")); !os.IsNotExist(err) {
		t.Fatalf("pod directory survived Close: %v", err)
	}
	if !e.Draining() {
		t.Fatal("Close must reject further creates")
	}
	if err := e.Create(ctx, samplePod("c", "uid-close-c"), Credentials{}); err == nil {
		t.Fatal("create after Close succeeded")
	}
	e.Close()
	if got := rt.Running(); got != 0 {
		t.Fatalf("second Close changed running VMs to %d", got)
	}
	if got := e.slots.Used(); got != 0 {
		t.Fatalf("second Close left %d slot(s)", got)
	}
}

func TestCloseSnapshotsCacheOfPodStillRunning(t *testing.T) {
	e, _ := closeTestEngine(t)
	pod := cachePod("c1", "uid-close-c1", map[string]string{"cache.darwin.node/xc": "/Users/mac/DD"})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "c1", corev1.PodRunning)
	hostDir := e.cachePodDir("uid-close-c1", "xc")
	if err := os.WriteFile(filepath.Join(hostDir, "state.bin"), []byte("close-me"), 0o644); err != nil {
		t.Fatal(err)
	}

	e.Close()

	got, err := os.ReadFile(filepath.Join(e.cacheStorePath("default", "xc"), "state.bin"))
	if err != nil || string(got) != "close-me" {
		t.Fatalf("cache snapshot = %q, %v", got, err)
	}
}
