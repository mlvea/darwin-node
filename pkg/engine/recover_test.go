package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

func TestRecoverCacheDeletesOldUnheld(t *testing.T) {
	cache := t.TempDir()
	oldDir := filepath.Join(cache, "pods", "old-uid")
	youngDir := filepath.Join(cache, "pods", "young-uid")
	heldDir := filepath.Join(cache, "pods", "held-uid")
	for _, d := range []string{oldDir, youngDir, heldDir} {
		if err := os.MkdirAll(filepath.Join(d, "volumes", "sec"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "volumes", "sec", "token"), []byte("shh"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(oldDir, stale, stale); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(heldDir, stale, stale); err != nil {
		t.Fatal(err)
	}

	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := slots.TryAcquire("held-uid"); err != nil {
		t.Fatal(err)
	}
	if err := RecoverCache(cache, slots, time.Hour); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("old unheld dir still present: %v", err)
	}
	if _, err := os.Stat(youngDir); err != nil {
		t.Fatalf("young dir swept: %v", err)
	}
	if _, err := os.Stat(heldDir); err != nil {
		t.Fatalf("held dir swept: %v", err)
	}
}

func TestRecoverCacheColdStart(t *testing.T) {
	cache := t.TempDir()
	oldDir := filepath.Join(cache, "pods", "orphan")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(oldDir, stale, stale); err != nil {
		t.Fatal(err)
	}
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	if slots.Used() != 0 {
		t.Fatal("expected empty slots")
	}
	if err := RecoverCache(cache, slots, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("stale dir still present: %v", err)
	}
}

func TestRecoverCacheMissingDir(t *testing.T) {
	if err := RecoverCache(filepath.Join(t.TempDir(), "nope"), nil, time.Hour); err != nil {
		t.Fatal(err)
	}
	if err := RecoverCache("", nil, time.Hour); err != nil {
		t.Fatal(err)
	}
}

func TestEngineNewSweepsStalePodDirs(t *testing.T) {
	cache := t.TempDir()
	oldDir := filepath.Join(cache, "pods", "stale-uid")
	youngDir := filepath.Join(cache, "pods", "young-uid")
	if err := os.MkdirAll(filepath.Join(oldDir, "volumes"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(youngDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldDir, "volumes", "token"), []byte("shh"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(oldDir, stale, stale); err != nil {
		t.Fatal(err)
	}

	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = cache
	cfg.AgentReadyTimeout = 5 * time.Second
	_ = New(cfg, slots, fake.New(), sidecar.None{}, event.Nop{}, "10.0.0.1")
	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Fatalf("New did not sweep stale uid dir: %v", err)
	}
	if _, err := os.Stat(youngDir); !os.IsNotExist(err) {
		t.Fatalf("young uid dir still present: %v", err)
	}
	if used := slots.Used(); used != 0 {
		t.Fatalf("orphan dirs pinned slots: used=%d", used)
	}
}

// A process restart sees every previous pod directory as unheld and young:
// the VMs died with the process, Drain may have given up, and nothing
// snapshotted the working tree. Secrets and overlays must go now. The
// committed cache store stays, and the slot table stays empty so the next
// create can use the 2-VM cap.
func TestNewDropsYoungOrphansWithoutPinningSlots(t *testing.T) {
	cache := t.TempDir()
	young := filepath.Join(cache, "pods", "young-uid")
	held := filepath.Join(cache, "pods", "held-uid")
	secret := filepath.Join(young, "volumes", "sec", "token")
	overlay := filepath.Join(young, "overlay", "disk.img")
	warmTok := filepath.Join(cache, "warm", "4", "control", types.GuestAgentTokenFile)
	store := filepath.Join(cache, cacheStoreDirName, "default", "derived", "state.bin")
	heldSecret := filepath.Join(held, "volumes", "sec", "token")
	for _, p := range []string{secret, overlay, warmTok, store, heldSecret} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("secret-bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	recent := time.Now().Add(-time.Minute)
	for _, d := range []string{young, held, filepath.Join(cache, "warm", "4")} {
		if err := os.Chtimes(d, recent, recent); err != nil {
			t.Fatal(err)
		}
	}

	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := slots.TryAcquire("held-uid"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = cache
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, fake.New(), sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)

	if _, err := os.Stat(secret); !os.IsNotExist(err) {
		t.Fatalf("young secret still on disk: %v", err)
	}
	if _, err := os.Stat(overlay); !os.IsNotExist(err) {
		t.Fatalf("young overlay still on disk: %v", err)
	}
	if _, err := os.Stat(warmTok); !os.IsNotExist(err) {
		t.Fatalf("previous process warm token still on disk: %v", err)
	}
	got, err := os.ReadFile(store)
	if err != nil || string(got) != "secret-bytes" {
		t.Fatalf("committed cache store = %q, %v", got, err)
	}
	if _, err := os.Stat(heldSecret); err != nil {
		t.Fatalf("held pod dir swept: %v", err)
	}
	if used := e.slots.Used(); used != 1 {
		t.Fatalf("slots used=%d, want 1 held and no pinned orphans", used)
	}
	if err := e.Create(context.Background(), samplePod("next", "uid-next"), Credentials{}); err != nil {
		t.Fatalf("orphan sweep pinned capacity: %v", err)
	}
	waitPhase(t, e, "default", "next", corev1.PodRunning)
	_ = e.Delete(context.Background(), "default", "next", 0)
}

func TestCreateRejectsHostPathRoot(t *testing.T) {
	e := testEngine(t)
	pod := samplePod("hp", "uid-hp")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "root",
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}},
	}}
	err := e.Create(context.Background(), pod, Credentials{})
	if err == nil {
		t.Fatal("expected Create to reject hostPath /")
	}
	if !strings.Contains(err.Error(), "hostPath") && !strings.Contains(err.Error(), "not allowed") && !strings.Contains(err.Error(), "host root") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type recEvents struct {
	mu    sync.Mutex
	warns []string
}

func (r *recEvents) Normal(context.Context, string, string) {}
func (r *recEvents) Warn(_ context.Context, reason, message string) {
	r.mu.Lock()
	r.warns = append(r.warns, reason+":"+message)
	r.mu.Unlock()
}

func TestCreateHostPathEmitsFailedCreate(t *testing.T) {
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	rec := &recEvents{}
	e := New(cfg, slots, fake.New(), sidecar.None{}, rec, "10.0.0.1")
	pod := samplePod("hp", "uid-hp2")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "root",
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}},
	}}
	if err := e.Create(context.Background(), pod, Credentials{}); err == nil {
		t.Fatal("expected error")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	found := false
	for _, w := range rec.warns {
		if strings.HasPrefix(w, event.ReasonFailedCreate+":") && strings.Contains(w, "root") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected FailedCreate event naming the volume, got %v", rec.warns)
	}
}

type dirCheckRuntime struct {
	inner      runtime.Runtime
	podDir     string
	mu         sync.Mutex
	stopSawDir bool
	stopCalled bool
}

func (r *dirCheckRuntime) Name() types.RuntimeName { return r.inner.Name() }

func (r *dirCheckRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.inner.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &dirCheckMachine{Machine: m, rt: r}, nil
}

type dirCheckMachine struct {
	runtime.Machine
	rt *dirCheckRuntime
}

func (m *dirCheckMachine) Stop(ctx context.Context, g time.Duration) error {
	m.rt.mu.Lock()
	m.rt.stopCalled = true
	_, err := os.Stat(m.rt.podDir)
	m.rt.stopSawDir = err == nil
	m.rt.mu.Unlock()
	return m.Machine.Stop(ctx, g)
}

func TestDeleteStopsBeforeRemoveAll(t *testing.T) {
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	uid := "uid-ord"
	podDir := filepath.Join(cfg.CacheDir, "pods", uid)
	rt := &dirCheckRuntime{inner: fake.New(), podDir: podDir}
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	pod := samplePod("ord", uid)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	_ = waitPhase(t, e, "default", "ord", corev1.PodRunning)
	if _, err := os.Stat(podDir); err != nil {
		t.Fatalf("pod dir missing before delete: %v", err)
	}
	if err := e.Delete(context.Background(), "default", "ord", 30); err != nil {
		t.Fatal(err)
	}
	rt.mu.Lock()
	saw, called := rt.stopSawDir, rt.stopCalled
	rt.mu.Unlock()
	if !called {
		t.Fatal("Stop was not called")
	}
	if !saw {
		t.Fatal("pod dir was gone before Stop returned")
	}
	if _, err := os.Stat(podDir); !os.IsNotExist(err) {
		t.Fatalf("pod dir still present after Delete: %v", err)
	}
}
