package engine

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// TestSucceedWorkloadCommitsCacheBeforeSucceededPhase is the race behind
// TestNamespaceAllowlistWritesCacheOnSuccess and
// TestEmptyServiceAccountMatchesDefaultWriter. succeedWorkload used to
// publish Succeeded and only then clone the pod cache into the store, so a
// waiter that treats Succeeded as "the cache is committed" read the store
// while that clone was still in flight and found no file.
func TestSucceedWorkloadCommitsCacheBeforeSucceededPhase(t *testing.T) {
	e, _ := cacheTestEngine(t, "default")
	pod := newCachePod("job", "uid-commit-order", "other", cacheAnnotations(cacheGuestPath, true), corev1.RestartPolicyNever)
	startCachePod(t, e, pod)
	writePodCache(t, e, "uid-commit-order", "xc", "state.bin", "built")

	e.mu.RLock()
	rec := e.pods["default/job"]
	e.mu.RUnlock()
	if rec == nil {
		t.Fatal("pod record missing")
	}
	code := int32(0)
	rec.mu.Lock()
	rec.guestExit = &code
	rec.guestExitReason = "Completed"
	rec.mu.Unlock()

	orig := cloneCacheTree
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	var releaseOnce sync.Once
	releaseCommit := func() { releaseOnce.Do(func() { close(release) }) }
	cloneCacheTree = func(src, dst string) error {
		startOnce.Do(func() { close(started) })
		<-release
		return orig(src, dst)
	}
	t.Cleanup(func() {
		releaseCommit()
		cloneCacheTree = orig
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.succeedWorkload(rec)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("cache commit did not start")
	}
	p, err := e.Get("default", "job")
	if err != nil {
		t.Fatal(err)
	}
	earlyPhase := p.Status.Phase == corev1.PodSucceeded
	_, statErr := os.Stat(filepath.Join(e.cacheStorePath("default", "xc"), "state.bin"))
	earlyFile := statErr == nil

	releaseCommit()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("succeedWorkload did not return after the cache commit was released")
	}
	if earlyPhase {
		t.Fatal("Succeeded was published while the cache commit was still blocked")
	}
	if earlyFile {
		t.Fatal("cache store file appeared while the commit clone was still blocked")
	}
	p, err = e.Get("default", "job")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("phase %s after the cache commit, want Succeeded", p.Status.Phase)
	}
	assertCacheBody(t, e, "default", "xc", "state.bin", "built")
}
