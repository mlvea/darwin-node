package engine

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestParallelSnapshotOfOneStoreDoesNotInterleave is the shared cache store
// under Close's parallel deletes. Two pods in one namespace can snapshot
// the same store at once. The clone and the rename have to be one at a time
// or the committed tree is torn.
func TestParallelSnapshotOfOneStoreDoesNotInterleave(t *testing.T) {
	cache := t.TempDir()
	e := engineOnCache(t, cache)
	orig := cloneCacheTree
	var current atomic.Int32
	var overlap atomic.Int32
	cloneCacheTree = func(src, dst string) error {
		if current.Add(1) > 1 {
			overlap.Add(1)
		}
		time.Sleep(40 * time.Millisecond)
		err := orig(src, dst)
		current.Add(-1)
		return err
	}
	t.Cleanup(func() { cloneCacheTree = orig })

	for _, uid := range []string{"uid-par-a", "uid-par-b"} {
		dir := e.cachePodDir(uid, "xc")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body := "from-" + uid
		if err := os.WriteFile(filepath.Join(dir, "state.bin"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	podA := cachePod("a", "uid-par-a", map[string]string{"cache.darwin.node/xc": "/Users/mac/DD"})
	podB := cachePod("b", "uid-par-b", map[string]string{"cache.darwin.node/xc": "/Users/mac/DD"})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		e.snapshotPodCaches(&podRecord{pod: podA})
	}()
	go func() {
		defer wg.Done()
		e.snapshotPodCaches(&podRecord{pod: podB})
	}()
	wg.Wait()
	if n := overlap.Load(); n != 0 {
		t.Fatalf("two deletes snapshotted one cache store at the same time (%d)", n)
	}
	store := e.cacheStorePath("default", "xc")
	assertNoCacheStaging(t, store)
	got, err := os.ReadFile(filepath.Join(store, "state.bin"))
	if err != nil {
		t.Fatalf("store missing after parallel snapshot: %v", err)
	}
	body := string(got)
	if body != "from-uid-par-a" && body != "from-uid-par-b" {
		t.Fatalf("store %q, want one pod's bytes", body)
	}
}
