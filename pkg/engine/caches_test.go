package engine

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

func cachePod(name, uid string, annotations map[string]string) *corev1.Pod {
	pod := samplePod(name, uid)
	pod.Annotations = annotations
	return pod
}

func TestParseCacheAnnotations(t *testing.T) {
	pod := cachePod("p", "u", map[string]string{
		"cache.darwin.node/derived": "/Users/mac/Library/Developer/Xcode/DerivedData",
		"cache.darwin.node/spm":     "/Users/mac/Library/Caches/org.swift.swiftpm",
	})
	caches, err := ParseCacheAnnotations(pod)
	if err != nil {
		t.Fatal(err)
	}
	if len(caches) != 2 || caches[0].Name != "derived" || caches[1].Name != "spm" {
		t.Fatalf("got %+v", caches)
	}
	if caches[0].GuestPath != "/Users/mac/Library/Developer/Xcode/DerivedData" {
		t.Fatalf("guest path %q", caches[0].GuestPath)
	}

	for name, tc := range map[string]map[string]string{
		"bad name":       {"cache.darwin.node/a b": "/x"},
		"empty name":     {"cache.darwin.node/": "/x"},
		"relative path":  {"cache.darwin.node/x": "relative/path"},
		"escaping path":  {"cache.darwin.node/x": "/a/../.."},
		"duplicate path": {"cache.darwin.node/x": "/same", "cache.darwin.node/y": "/same"},
	} {
		if _, err := ParseCacheAnnotations(cachePod("p", "u", tc)); err == nil {
			t.Fatalf("%s: expected error", name)
		}
	}

	if caches, err := ParseCacheAnnotations(cachePod("p", "u", nil)); err != nil || caches != nil {
		t.Fatalf("no annotations: %+v %v", caches, err)
	}
}

func TestPrepareCachesRestoresFromStore(t *testing.T) {
	e := testEngine(t)
	pod := cachePod("c", "uid-cache", map[string]string{"cache.darwin.node/xc": "/Users/mac/DD"})

	store := e.cacheStorePath("default", "xc")
	if err := os.MkdirAll(filepath.Join(store, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "warm.txt"), []byte("warm"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "sub", "deep.txt"), []byte("deep"), 0o644); err != nil {
		t.Fatal(err)
	}

	shares, places, err := e.prepareCaches(pod)
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 1 || shares[0].Name != "cache.darwin.node.xc" || shares[0].ReadOnly {
		t.Fatalf("shares %+v", shares)
	}
	if len(places) != 1 || places[0].GuestPath != "/Users/mac/DD" || places[0].Mode != "link" {
		t.Fatalf("places %+v", places)
	}

	hostDir := e.cachePodDir(string(pod.UID), "xc")
	b, err := os.ReadFile(filepath.Join(hostDir, "warm.txt"))
	if err != nil || string(b) != "warm" {
		t.Fatalf("restore missing warm.txt: %q %v", b, err)
	}
	b, err = os.ReadFile(filepath.Join(hostDir, "sub", "deep.txt"))
	if err != nil || string(b) != "deep" {
		t.Fatalf("nested restore failed: %q %v", b, err)
	}
}

func TestCacheSnapshotRoundTripThroughLifecycle(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	ann := map[string]string{"cache.darwin.node/xc": "/Users/mac/DD"}

	first := cachePod("ci-1", "uid-ci-1", ann)
	if err := e.Create(ctx, first, Credentials{}); err != nil {
		t.Fatal(err)
	}
	_ = waitPhase(t, e, "default", "ci-1", corev1.PodRunning)

	hostDir := e.cachePodDir("uid-ci-1", "xc")
	if _, err := os.Stat(hostDir); err != nil {
		t.Fatalf("cache dir missing: %v", err)
	}
	// Simulate guest writes flowing through virtio-fs to the host dir.
	if err := os.WriteFile(filepath.Join(hostDir, "derived.bin"), []byte("build-output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := e.Delete(ctx, "default", "ci-1", 0); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(filepath.Join(e.cacheStorePath("default", "xc"), "derived.bin"))
	if err != nil || string(b) != "build-output" {
		t.Fatalf("snapshot not saved: %q %v", b, err)
	}

	// A fresh pod in the same namespace starts with the cached state.
	second := cachePod("ci-2", "uid-ci-2", ann)
	if err := e.Create(ctx, second, Credentials{}); err != nil {
		t.Fatal(err)
	}
	_ = waitPhase(t, e, "default", "ci-2", corev1.PodRunning)
	b, err = os.ReadFile(filepath.Join(e.cachePodDir("uid-ci-2", "xc"), "derived.bin"))
	if err != nil || string(b) != "build-output" {
		t.Fatalf("second pod did not restore cache: %q %v", b, err)
	}
	_ = e.Delete(ctx, "default", "ci-2", 0)
}

func TestFailedStartDoesNotSnapshotCaches(t *testing.T) {
	slotTable, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	rt := fake.New()
	rt.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "false" {
			return 1, nil
		}
		return 0, nil
	}
	e := New(cfg, slotTable, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)

	pod := cachePod("cf", "uid-cf", map[string]string{"cache.darwin.node/xc": "/Users/mac/DD"})
	pod.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{
		PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"false"}}},
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	_ = waitPhase(t, e, "default", "cf", corev1.PodFailed)
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(e.cacheStorePath("default", "xc")); !os.IsNotExist(err) {
		t.Fatalf("failed pod must not snapshot caches (err=%v)", err)
	}
	_ = e.Delete(context.Background(), "default", "cf", 0)
}
