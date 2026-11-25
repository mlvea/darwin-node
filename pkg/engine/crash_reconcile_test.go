package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/digest"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/image"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

func TestNewDropsUnreadableAbandonedBootsRecord(t *testing.T) {
	cache := t.TempDir()
	orphan := filepath.Join(cache, "pods", "orphan-uid", "volumes", "token")
	if err := os.MkdirAll(filepath.Dir(orphan), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphan, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A crash during WriteFile truncates the record mid-array.
	record := filepath.Join(cache, abandonedBootsFile)
	if err := os.WriteFile(record, []byte(`[{"id":"orphan-uid"`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newCrashEngine(t, cache)
	if _, err := os.Stat(record); !os.IsNotExist(err) {
		t.Fatalf("abandoned record still present: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan bundle still present: %v", err)
	}
	if used := e.slots.Used(); used != 0 {
		t.Fatalf("slots used=%d, corrupt record pinned a slot", used)
	}
	if err := e.Create(context.Background(), samplePod("next", "uid-corrupt-next"), Credentials{}); err != nil {
		t.Fatalf("free slot was pinned: %v", err)
	}
	waitPhase(t, e, "default", "next", corev1.PodRunning)
}

func TestNewDropsPartialAbandonedBootsWrite(t *testing.T) {
	cache := t.TempDir()
	partial := filepath.Join(cache, abandonedBootsFile+".tmp")
	if err := os.WriteFile(partial, []byte(`[{"id":"half"`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := newCrashEngine(t, cache)
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial abandoned boot record still present: %v", err)
	}
	if used := e.slots.Used(); used != 0 {
		t.Fatalf("slots used=%d", used)
	}
}

func TestSweepConsoleSockets(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "darwin-console-0123456789abcdef.sock")
	other := filepath.Join(dir, "not-a-console.sock")
	keep := filepath.Join(dir, "darwin-console-short.sock")
	for _, p := range []string{stale, other, keep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := SweepConsoleSockets(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("stale console socket still present: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("unrelated socket removed: %v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("non-matching console name removed: %v", err)
	}
}

func TestNewHealsImageCacheAfterCrash(t *testing.T) {
	cache := t.TempDir()
	ref := "example.com/macos:v1"
	key := image.CacheKey(ref)
	backup := filepath.Join(cache, "image-replacing", key)
	partial := filepath.Join(cache, "image-partial", key, "disk.img")
	plantCommittedImage(t, backup, "committed-disk")
	if err := os.MkdirAll(filepath.Dir(partial), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, []byte("SHORT"), 0o600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cache, "image-partial", "escape")); err != nil {
		t.Fatal(err)
	}

	_ = newCrashEngine(t, cache)

	final := image.CacheDir(cache, ref)
	img, err := image.LoadCommitted(final)
	if err != nil {
		t.Fatalf("committed image was not restored: %v", err)
	}
	got, err := os.ReadFile(img.DiskPath)
	if err != nil || string(got) != "committed-disk" {
		t.Fatalf("restored disk %q err=%v", got, err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("image backup still present: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(partial)); !os.IsNotExist(err) {
		t.Fatalf("image partial still present: %v", err)
	}
	body, err := os.ReadFile(outside)
	if err != nil || string(body) != "keep" {
		t.Fatalf("recovery followed an image symlink: %q %v", body, err)
	}
	if _, err := os.Lstat(filepath.Join(cache, "image-partial", "escape")); !os.IsNotExist(err) {
		t.Fatalf("symlink left in image-partial: %v", err)
	}
}

func TestRecoverCacheStoreDoesNotFollowSymlink(t *testing.T) {
	cache := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cache, cacheStoreDirName, "default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "xc.tmp")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "yc.old")); err != nil {
		t.Fatal(err)
	}
	_ = newCrashEngine(t, cache)
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "keep" {
		t.Fatalf("cache recovery followed a symlink: %q %v", got, err)
	}
	for _, name := range []string{"xc.tmp", "yc.old"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("cache symlink %s still present: %v", name, err)
		}
	}
}

func TestNewDoesNotSweepThroughPodOrStoreSymlink(t *testing.T) {
	cache := t.TempDir()
	outside := t.TempDir()
	podVictim := filepath.Join(outside, "pods", "victim", "secret")
	storeVictim := filepath.Join(outside, "stores", "default", "token.tmp", "secret")
	for _, p := range []string{podVictim, storeVictim} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(outside, "pods"), filepath.Join(cache, "pods")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "stores"), filepath.Join(cache, cacheStoreDirName)); err != nil {
		t.Fatal(err)
	}
	_ = newCrashEngine(t, cache)
	for _, p := range []string{podVictim, storeVictim} {
		got, err := os.ReadFile(p)
		if err != nil || string(got) != "keep" {
			t.Fatalf("recovery escaped the cache at %s: %q %v", p, got, err)
		}
	}
}

func newCrashEngine(t *testing.T, cache string) *Engine {
	t.Helper()
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = cache
	cfg.AgentReadyTimeout = 2 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, fake.New(), sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func plantCommittedImage(t *testing.T, dir, disk string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{
		"os":"darwin",
		"hardwareModelData":"abc",
		"machineIdData":"def",
		"storage":[
			{"mediatype":"application/vnd.darwin-node.disk.v1","file":"disk.img"},
			{"mediatype":"application/vnd.darwin-node.aux.v1","file":"aux.img"}
		]
	}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte(disk), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux.img"), []byte("AUX"), 0o644); err != nil {
		t.Fatal(err)
	}
	diskSum, err := digest.FileSHA256(filepath.Join(dir, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	auxSum, err := digest.FileSHA256(filepath.Join(dir, "aux.img"))
	if err != nil {
		t.Fatal(err)
	}
	if err := image.WriteProvenance(dir, image.Provenance{
		Files: map[string]string{"disk.img": diskSum.String(), "aux.img": auxSum.String()},
	}); err != nil {
		t.Fatal(err)
	}
}
