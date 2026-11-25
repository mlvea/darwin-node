package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

const cacheGuestPath = "/Users/mac/DD"

func cacheTestEngine(t *testing.T, writers ...string) (*Engine, *fake.Runtime) {
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
	cfg.ProbeInterval = 20 * time.Millisecond
	cfg.CacheWriters = writers
	rt := fake.New()
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(func() { _ = e.Close() })
	return e, rt
}

func cacheAnnotations(path string, write bool) map[string]string {
	ann := map[string]string{"cache.darwin.node/xc": path}
	if write {
		ann["cache-write.darwin.node/xc"] = "true"
	}
	return ann
}

func newCachePod(name, uid, sa string, ann map[string]string, policy corev1.RestartPolicy) *corev1.Pod {
	pod := cachePod(name, uid, ann)
	pod.Spec.RestartPolicy = policy
	pod.Spec.ServiceAccountName = sa
	return pod
}

func seedCacheFile(t *testing.T, e *Engine, ns, name, file, body string) {
	t.Helper()
	dir := e.cacheStorePath(ns, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writePodCache(t *testing.T, e *Engine, uid, name, file, body string) {
	t.Helper()
	dir := e.cachePodDir(uid, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertCacheBody(t *testing.T, e *Engine, ns, name, file, want string) {
	t.Helper()
	got, err := os.ReadFile(filepath.Join(e.cacheStorePath(ns, name), file))
	if want == "" {
		if !os.IsNotExist(err) {
			t.Fatalf("cache store %s/%s/%s = %q (%v); want it absent", ns, name, file, got, err)
		}
		return
	}
	if err != nil || string(got) != want {
		t.Fatalf("cache store %s/%s/%s = %q (%v); want %q", ns, name, file, got, err, want)
	}
}

func startCachePod(t *testing.T, e *Engine, pod *corev1.Pod) {
	t.Helper()
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, pod.Namespace, pod.Name, corev1.PodRunning)
}

func endWorkload(t *testing.T, e *Engine, rt *fake.Runtime, ns, name string, code int32) {
	t.Helper()
	reason := "Completed"
	if code != 0 {
		reason = "Error"
	}
	rt.ReportExit(guest.ExitReport{ExitCode: &code, Reason: reason})
	waitGuestExit(t, e, ns+"/"+name, code)
	if err := podMachine(t, e, ns+"/"+name).Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	want := corev1.PodSucceeded
	if code != 0 {
		want = corev1.PodFailed
	}
	waitPhase(t, e, ns, name, want)
}

func TestUntrustedDeleteDoesNotPoisonNamedCache(t *testing.T) {
	e, _ := cacheTestEngine(t)
	seedCacheFile(t, e, "default", "xc", "state.bin", "good")
	pod := newCachePod("bad", "uid-poison", "", cacheAnnotations(cacheGuestPath, true), corev1.RestartPolicyNever)
	startCachePod(t, e, pod)
	writePodCache(t, e, "uid-poison", "xc", "state.bin", "poison")
	if err := e.Delete(context.Background(), "default", "bad", 30); err != nil {
		t.Fatal(err)
	}
	assertCacheBody(t, e, "default", "xc", "state.bin", "good")

	next := newCachePod("next", "uid-next", "", cacheAnnotations(cacheGuestPath, false), corev1.RestartPolicyNever)
	startCachePod(t, e, next)
	got, err := os.ReadFile(filepath.Join(e.cachePodDir("uid-next", "xc"), "state.bin"))
	if err != nil || string(got) != "good" {
		t.Fatalf("next pod restored %q (%v); the untrusted pod published its bytes", got, err)
	}
}

func TestCacheConsumerShareIsReadOnly(t *testing.T) {
	e, _ := cacheTestEngine(t)
	pod := newCachePod("c", "uid-ro", "", cacheAnnotations(cacheGuestPath, false), corev1.RestartPolicyNever)
	shares, places, err := e.prepareCaches(pod)
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 1 || !shares[0].ReadOnly || shares[0].Name != "cache.darwin.node.xc" {
		t.Fatalf("consumer share %+v, want a read-only cache share", shares)
	}
	if len(places) != 1 || !places[0].ReadOnly || places[0].GuestPath != cacheGuestPath {
		t.Fatalf("consumer placement %+v, want read-only", places)
	}
}

func TestWritableCacheIsReadWriteAndSiblingStaysReadOnly(t *testing.T) {
	e, _ := cacheTestEngine(t, "default/builder")
	pod := newCachePod("c", "uid-mix", "builder", map[string]string{
		"cache.darwin.node/xc":        cacheGuestPath,
		"cache.darwin.node/spm":       "/Users/mac/SPM",
		"cache-write.darwin.node/xc":  "true",
		"cache-write.darwin.node/spm": "false",
	}, corev1.RestartPolicyNever)
	shares, places, err := e.prepareCaches(pod)
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 2 || len(places) != 2 {
		t.Fatalf("shares %+v places %+v", shares, places)
	}
	shareRO := map[string]bool{}
	for _, s := range shares {
		shareRO[s.Name] = s.ReadOnly
	}
	placeRO := map[string]bool{}
	for _, p := range places {
		placeRO[p.Name] = p.ReadOnly
	}
	if shareRO["cache.darwin.node.xc"] || placeRO["cache.darwin.node.xc"] {
		t.Fatalf("writable cache was read-only: shares %+v places %+v", shares, places)
	}
	if !shareRO["cache.darwin.node.spm"] || !placeRO["cache.darwin.node.spm"] {
		t.Fatalf("cache without write permission was read-write: shares %+v places %+v", shares, places)
	}
}

func TestSuccessfulExitSnapshotsCacheBeforePodDirectoryIsRemoved(t *testing.T) {
	e, rt := cacheTestEngine(t, "default")
	pod := newCachePod("job", "uid-job", "builder", cacheAnnotations(cacheGuestPath, true), corev1.RestartPolicyNever)
	startCachePod(t, e, pod)
	writePodCache(t, e, "uid-job", "xc", "state.bin", "built")
	endWorkload(t, e, rt, "default", "job", 0)

	assertCacheBody(t, e, "default", "xc", "state.bin", "built")
	if err := os.RemoveAll(filepath.Join(e.cfg.CacheDir, "pods", "uid-job")); err != nil {
		t.Fatal(err)
	}
	assertCacheBody(t, e, "default", "xc", "state.bin", "built")
	assertNoCacheStaging(t, e.cacheStorePath("default", "xc"))
}

func TestExitZeroWritesCacheBeforeRestartReplacesIt(t *testing.T) {
	e, rt := cacheTestEngine(t, "default")
	pod := newCachePod("loop", "uid-loop", "", cacheAnnotations(cacheGuestPath, true), corev1.RestartPolicyAlways)
	startCachePod(t, e, pod)
	writePodCache(t, e, "uid-loop", "xc", "state.bin", "built")
	code := int32(0)
	rt.ReportExit(guest.ExitReport{ExitCode: &code, Reason: "Completed"})
	waitGuestExit(t, e, "default/loop", 0)

	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := os.ReadFile(filepath.Join(e.cacheStorePath("default", "xc"), "state.bin"))
		if err == nil && string(got) == "built" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("exit 0 did not snapshot before the pod was deleted or the directory removed: %q %v", got, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestInterruptedWriterDoesNotReplaceCache(t *testing.T) {
	e, _ := cacheTestEngine(t, "default/builder")
	seedCacheFile(t, e, "default", "xc", "state.bin", "good")
	pod := newCachePod("job", "uid-kill", "builder", cacheAnnotations(cacheGuestPath, true), corev1.RestartPolicyNever)
	startCachePod(t, e, pod)
	writePodCache(t, e, "uid-kill", "xc", "state.bin", "partial")
	if err := e.Delete(context.Background(), "default", "job", 30); err != nil {
		t.Fatal(err)
	}
	assertCacheBody(t, e, "default", "xc", "state.bin", "good")
	assertNoCacheStaging(t, e.cacheStorePath("default", "xc"))
}

func TestFailedExitDoesNotReplaceCache(t *testing.T) {
	e, rt := cacheTestEngine(t, "default")
	seedCacheFile(t, e, "default", "xc", "state.bin", "good")
	pod := newCachePod("job", "uid-fail", "builder", cacheAnnotations(cacheGuestPath, true), corev1.RestartPolicyNever)
	startCachePod(t, e, pod)
	writePodCache(t, e, "uid-fail", "xc", "state.bin", "corrupt")
	code := int32(7)
	rt.ReportExit(guest.ExitReport{ExitCode: &code, Reason: "Error"})
	waitGuestExit(t, e, "default/job", 7)
	if err := e.Delete(context.Background(), "default", "job", 30); err != nil {
		t.Fatal(err)
	}
	assertCacheBody(t, e, "default", "xc", "state.bin", "good")
	assertNoCacheStaging(t, e.cacheStorePath("default", "xc"))
}

func TestServiceAccountOutsideAllowlistCannotWriteCache(t *testing.T) {
	e, rt := cacheTestEngine(t, "default/builder")
	seedCacheFile(t, e, "default", "xc", "state.bin", "good")
	pod := newCachePod("job", "uid-other", "other", cacheAnnotations(cacheGuestPath, true), corev1.RestartPolicyNever)
	startCachePod(t, e, pod)
	writePodCache(t, e, "uid-other", "xc", "state.bin", "poison")
	endWorkload(t, e, rt, "default", "job", 0)
	assertCacheBody(t, e, "default", "xc", "state.bin", "good")
}

func TestAllowlistedServiceAccountWritesCacheOnSuccess(t *testing.T) {
	e, rt := cacheTestEngine(t, "default/builder")
	pod := newCachePod("job", "uid-builder", "builder", cacheAnnotations(cacheGuestPath, true), corev1.RestartPolicyNever)
	startCachePod(t, e, pod)
	writePodCache(t, e, "uid-builder", "xc", "state.bin", "built")
	endWorkload(t, e, rt, "default", "job", 0)
	assertCacheBody(t, e, "default", "xc", "state.bin", "built")
}

func TestEmptyServiceAccountMatchesDefaultWriter(t *testing.T) {
	e, rt := cacheTestEngine(t, "default/default")
	pod := newCachePod("job", "uid-sa-default", "", cacheAnnotations(cacheGuestPath, true), corev1.RestartPolicyNever)
	startCachePod(t, e, pod)
	writePodCache(t, e, "uid-sa-default", "xc", "state.bin", "built")
	endWorkload(t, e, rt, "default", "job", 0)
	assertCacheBody(t, e, "default", "xc", "state.bin", "built")
}

func TestNamespaceAllowlistWritesCacheOnSuccess(t *testing.T) {
	e, rt := cacheTestEngine(t, "default")
	pod := newCachePod("job", "uid-ns", "other", cacheAnnotations(cacheGuestPath, true), corev1.RestartPolicyNever)
	startCachePod(t, e, pod)
	writePodCache(t, e, "uid-ns", "xc", "state.bin", "built")
	endWorkload(t, e, rt, "default", "job", 0)
	assertCacheBody(t, e, "default", "xc", "state.bin", "built")
}

func TestAllowlistedPodWithoutWriteAnnotationDoesNotSnapshot(t *testing.T) {
	e, rt := cacheTestEngine(t, "default")
	pod := newCachePod("job", "uid-nowrite", "builder", cacheAnnotations(cacheGuestPath, false), corev1.RestartPolicyNever)
	shares, _, err := e.prepareCaches(pod)
	if err != nil {
		t.Fatal(err)
	}
	if len(shares) != 1 || !shares[0].ReadOnly {
		t.Fatalf("share %+v, an allowlisted pod without cache-write stays read-only", shares)
	}
	startCachePod(t, e, pod)
	writePodCache(t, e, "uid-nowrite", "xc", "state.bin", "built")
	endWorkload(t, e, rt, "default", "job", 0)
	assertCacheBody(t, e, "default", "xc", "state.bin", "")
}

func TestCacheWriteAnnotationMustNameADeclaredCache(t *testing.T) {
	e, _ := cacheTestEngine(t, "default")
	pod := newCachePod("job", "uid-missing", "", map[string]string{
		"cache-write.darwin.node/xc": "true",
	}, corev1.RestartPolicyNever)
	if err := e.Create(context.Background(), pod, Credentials{}); err == nil {
		t.Fatal("write annotation without a declared cache was accepted")
	}
	if e.slots.Used() != 0 {
		t.Fatalf("rejected pod held a slot: %d", e.slots.Used())
	}
}

func TestCacheWriteAnnotationRejectsNonBoolean(t *testing.T) {
	e, _ := cacheTestEngine(t, "default")
	pod := newCachePod("job", "uid-badval", "", map[string]string{
		"cache.darwin.node/xc":       cacheGuestPath,
		"cache-write.darwin.node/xc": "yes",
	}, corev1.RestartPolicyNever)
	if err := e.Create(context.Background(), pod, Credentials{}); err == nil {
		t.Fatal(`cache-write value "yes" was accepted`)
	}
}

func TestOnlyTheWritableCacheIsSnapshotted(t *testing.T) {
	e, rt := cacheTestEngine(t, "ci/publisher")
	pod := newCachePod("job", "uid-one", "publisher", map[string]string{
		"cache.darwin.node/xc":       "/Users/mac/DD",
		"cache.darwin.node/spm":      "/Users/mac/SPM",
		"cache-write.darwin.node/xc": "true",
	}, corev1.RestartPolicyNever)
	pod.Namespace = "ci"
	startCachePod(t, e, pod)
	writePodCache(t, e, "uid-one", "xc", "state.bin", "built")
	writePodCache(t, e, "uid-one", "spm", "state.bin", "secret")
	endWorkload(t, e, rt, "ci", "job", 0)
	assertCacheBody(t, e, "ci", "xc", "state.bin", "built")
	assertCacheBody(t, e, "ci", "spm", "state.bin", "")
}

func TestSnapshotCommitKeepsPreviousStoreWhenCloneFails(t *testing.T) {
	e, _ := cacheTestEngine(t, "default")
	seedCacheFile(t, e, "default", "xc", "state.bin", "good")
	pod := newCachePod("job", "uid-clone", "builder", cacheAnnotations(cacheGuestPath, true), corev1.RestartPolicyNever)
	if err := os.MkdirAll(e.cachePodDir("uid-clone", "xc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writePodCache(t, e, "uid-clone", "xc", "state.bin", "new")
	orig := cloneCacheTree
	var calls int
	cloneCacheTree = func(src, dst string) error {
		calls++
		return os.ErrPermission
	}
	t.Cleanup(func() { cloneCacheTree = orig })
	code := int32(0)
	e.snapshotPodCaches(&podRecord{pod: pod, guestExit: &code})
	if calls != 1 {
		t.Fatalf("clone attempts %d, a trusted exit 0 must try to commit and then leave the previous store", calls)
	}
	assertCacheBody(t, e, "default", "xc", "state.bin", "good")
	assertNoCacheStaging(t, e.cacheStorePath("default", "xc"))
}
