package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/image"
	rtfake "github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type gcRec struct {
	events [][2]string
}

func (g *gcRec) Normal(_ context.Context, reason, message string) {
	g.events = append(g.events, [2]string{reason, message})
}

func (g *gcRec) Warn(_ context.Context, reason, message string) {
	g.events = append(g.events, [2]string{reason, message})
}

func gcEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	cfg := config.Default()
	cfg.CacheDir = root
	cfg.Runtime = types.RuntimeFake
	cfg.WarmSlots = 0
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	rec := &gcRec{}
	e := New(cfg, slots, rtfake.New(), sidecar.None{}, rec, "10.0.0.8")
	return e, root
}

func plantImage(t *testing.T, root, ref, base string, mod time.Time) string {
	t.Helper()
	dir := filepath.Join(root, "images", image.CacheKey(ref))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, image.RefFile), []byte(ref+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if base != "" {
		if err := image.WriteDeltaManifest(dir, image.DeltaManifest{BaseImage: base}); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(dir, mod, mod); err != nil {
		t.Fatal(err)
	}
	return dir
}

func plantStore(t *testing.T, root, ns, name string, mod time.Time) string {
	t.Helper()
	dir := filepath.Join(root, cacheStoreDirName, ns, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, mod, mod); err != nil {
		t.Fatal(err)
	}
	return dir
}

func pinPod(e *Engine, ns, name, img string, phase corev1.PodPhase, ann map[string]string) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Annotations: ann},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: img}}},
	}
	e.mu.Lock()
	e.pods[Key(ns, name)] = &podRecord{pod: pod, phase: phase}
	e.mu.Unlock()
}

func TestGarbageCollectStaysIdleBelowHigh(t *testing.T) {
	e, root := gcEngine(t)
	old := time.Now().Add(-time.Hour)
	dir := plantImage(t, root, "local/unused:1", "", old)
	called := 0
	err := e.collectGarbage(context.Background(), func(string) (float64, error) {
		called++
		return 0.50, nil
	}, func(string) error { t.Fatal("deleted below the high threshold"); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 {
		t.Fatalf("usage calls %d", called)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

func TestGarbageCollectDisabledDoesNotStat(t *testing.T) {
	e, _ := gcEngine(t)
	e.cfg.ImageGC = false
	err := e.collectGarbage(context.Background(), func(string) (float64, error) {
		t.Fatal("usage was read while image GC is off")
		return 1, nil
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
}

func TestGarbageCollectStopsAtLowAndDeletesOldest(t *testing.T) {
	e, root := gcEngine(t)
	old := time.Now().Add(-2 * time.Hour)
	newer := old.Add(time.Hour)
	unused := plantImage(t, root, "local/unused:1", "", old)
	kept := plantImage(t, root, "local/kept:1", "", newer)
	reads := []float64{0.95, 0.95, 0.50}
	n := 0
	var removed []string
	err := e.collectGarbage(context.Background(), func(string) (float64, error) {
		i := n
		if i >= len(reads) {
			i = len(reads) - 1
		}
		n++
		return reads[i], nil
	}, func(path string) error {
		removed = append(removed, path)
		return os.RemoveAll(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != unused {
		t.Fatalf("removed %v, want only %s", removed, unused)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Fatal(err)
	}
	rec := e.events.(*gcRec)
	if len(rec.events) != 1 || rec.events[0][0] != event.ReasonImageEvicted {
		t.Fatalf("events %+v", rec.events)
	}
	if rec.events[0][1] != "evicted image local/unused:1" {
		t.Fatalf("event %q", rec.events[0][1])
	}
}

func TestGarbageCollectDeletesDeltaBeforeItsBase(t *testing.T) {
	e, root := gcEngine(t)
	baseTime := time.Now().Add(-3 * time.Hour)
	deltaTime := baseTime.Add(time.Hour)
	base := plantImage(t, root, "local/base:1", "", baseTime)
	delta := plantImage(t, root, "local/delta:1", "local/base:1", deltaTime)
	var removed []string
	err := e.collectGarbage(context.Background(), func(string) (float64, error) { return 0.99, nil }, func(path string) error {
		removed = append(removed, path)
		if len(removed) > 4 {
			return errors.New("gc did not stop")
		}
		return os.RemoveAll(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 || removed[0] != delta || removed[1] != base {
		t.Fatalf("order %v, want delta %s then base %s", removed, delta, base)
	}
}

func TestGarbageCollectKeepsPinnedImagesAndReferencedBases(t *testing.T) {
	e, root := gcEngine(t)
	old := time.Now().Add(-4 * time.Hour)
	base := plantImage(t, root, "local/base:1", "", old)
	delta := plantImage(t, root, "local/delta:1", "local/base:1", old.Add(time.Minute))
	pending := plantImage(t, root, "local/pending:1", "", old.Add(2*time.Minute))
	running := plantImage(t, root, "local/running:1", "", old.Add(3*time.Minute))
	failed := plantImage(t, root, "local/failed:1", "", old.Add(4*time.Minute))
	warm := plantImage(t, root, "local/warm:1", "", old.Add(5*time.Minute))
	pinPod(e, "default", "delta", "local/delta:1", corev1.PodPending, nil)
	pinPod(e, "default", "pend", "local/pending:1", corev1.PodPending, nil)
	pinPod(e, "default", "run", "local/running:1", corev1.PodRunning, map[string]string{
		types.AnnotationCachePrefix + "build": "/var/cache/build",
	})
	pinPod(e, "default", "fail", "local/failed:1", corev1.PodFailed, nil)
	store := plantStore(t, root, "default", "build", old)
	freeStore := plantStore(t, root, "default", "scratch", old.Add(time.Minute))
	staging := plantStore(t, root, "default", "scratch.tmp", old)
	if !e.warm.addIfOpen(&warmEntry{imgRef: "local/warm:1"}) {
		t.Fatal("warm pool rejected the entry")
	}
	err := e.collectGarbage(context.Background(), func(string) (float64, error) { return 0.99, nil }, os.RemoveAll)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{base, delta, pending, running, warm, store, staging} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("failed pod image still present: %v", err)
	}
	if _, err := os.Stat(freeStore); !os.IsNotExist(err) {
		t.Fatalf("unpinned cache store still present: %v", err)
	}
	rec := e.events.(*gcRec)
	var sawCache bool
	for _, ev := range rec.events {
		if ev[0] == event.ReasonCacheEvicted && ev[1] == "evicted cache default/scratch" {
			sawCache = true
		}
	}
	if !sawCache {
		t.Fatalf("events %+v", rec.events)
	}
}

func TestGarbageCollectSkipsSymlinksAndPartials(t *testing.T) {
	e, root := gcEngine(t)
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "images", image.CacheKey("local/link:1"))
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(root, "image-partial", image.CacheKey("local/partial:1"))
	if err := os.MkdirAll(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	err := e.collectGarbage(context.Background(), func(string) (float64, error) { return 0.99, nil }, os.RemoveAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(secret); err != nil {
		t.Fatalf("symlink target: %v", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("image symlink: %v", err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatalf("partial: %v", err)
	}
}

func TestGarbageCollectUsageErrorDeletesNothing(t *testing.T) {
	e, root := gcEngine(t)
	dir := plantImage(t, root, "local/unused:1", "", time.Now().Add(-time.Hour))
	err := e.collectGarbage(context.Background(), func(string) (float64, error) {
		return 0, errors.New("statfs failed")
	}, func(string) error { t.Fatal("deleted after a usage error"); return nil })
	if err == nil {
		t.Fatal("usage error was ignored")
	}
	if _, statErr := os.Stat(dir); statErr != nil {
		t.Fatal(statErr)
	}
}

func TestRemoveWithinRefusesEscapeAndSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := removeWithin(root, outside); err == nil {
		t.Fatal("deleted a path outside the cache")
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := removeWithin(root, link); err == nil {
		t.Fatal("deleted a symlink")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal(err)
	}
}

func TestGarbageCollectKeepsACycleAndASelfReferenceCanGo(t *testing.T) {
	e, root := gcEngine(t)
	old := time.Now().Add(-time.Hour)
	a := plantImage(t, root, "local/a:1", "local/b:1", old)
	b := plantImage(t, root, "local/b:1", "local/a:1", old.Add(time.Minute))
	self := plantImage(t, root, "local/self:1", "local/self:1", old)
	err := e.collectGarbage(context.Background(), func(string) (float64, error) { return 0.99, nil }, os.RemoveAll)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(b); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(self); !os.IsNotExist(err) {
		t.Fatalf("self-reference stayed: %v", err)
	}
}

func TestWarmImageIsPinnedOnlyWhileThePoolIsConfigured(t *testing.T) {
	e, root := gcEngine(t)
	dir := plantImage(t, root, "local/warm-cfg:1", "", time.Now().Add(-time.Hour))
	e.cfg.WarmImage = "local/warm-cfg:1"
	e.cfg.WarmSlots = 0
	if err := e.collectGarbage(context.Background(), func(string) (float64, error) { return 0.99, nil }, os.RemoveAll); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("warm image was pinned with WarmSlots 0")
	}
	dir = plantImage(t, root, "local/warm-cfg:1", "", time.Now().Add(-time.Hour))
	e.cfg.WarmSlots = 1
	if err := e.collectGarbage(context.Background(), func(string) (float64, error) { return 0.99, nil }, os.RemoveAll); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("configured warm image was deleted: %v", err)
	}
}
