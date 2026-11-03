package engine

import (
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
)

// cacheCrashCases are the on-disk shapes snapshotPodCaches can leave behind
// if the process dies between steps. want is the last committed body;
// empty want means nothing was ever committed and must not be published.
func cacheCrashCases() []struct {
	name string
	tree map[string]string
	want string
} {
	return []struct {
		name string
		tree map[string]string
		want string
	}{
		{
			name: "between renames",
			tree: map[string]string{
				"xc.old/state.bin": "committed-snapshot",
				"xc.tmp/state.bin": "inflight-uncommitted",
			},
			want: "committed-snapshot",
		},
		{
			name: "tmp leftover beside live store",
			tree: map[string]string{
				"xc/state.bin":     "committed-snapshot",
				"xc.tmp/state.bin": "partial-or-inflight",
			},
			want: "committed-snapshot",
		},
		{
			name: "old leftover after commit",
			tree: map[string]string{
				"xc/state.bin":     "published-snapshot",
				"xc.old/state.bin": "previous-snapshot",
			},
			want: "published-snapshot",
		},
		{
			name: "tmp and old beside live store",
			tree: map[string]string{
				"xc/state.bin":     "published-snapshot",
				"xc.old/state.bin": "previous-snapshot",
				"xc.tmp/state.bin": "partial-or-inflight",
			},
			want: "published-snapshot",
		},
		{
			name: "old only",
			tree: map[string]string{
				"xc.old/state.bin": "committed-snapshot",
			},
			want: "committed-snapshot",
		},
		{
			name: "tmp only",
			tree: map[string]string{
				"xc.tmp/state.bin": "partial-bytes",
			},
			want: "",
		},
	}
}

func engineOnCache(t *testing.T, cache string) *Engine {
	t.Helper()
	leakcheck.Check(t)
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = cache
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, fake.New(), sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)
	return e
}

func plantCacheTree(t *testing.T, cache string, files map[string]string) {
	t.Helper()
	root := filepath.Join(cache, cacheStoreDirName, "default")
	for rel, body := range files {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func assertNoCacheStaging(t *testing.T, store string) {
	t.Helper()
	for _, suf := range []string{".tmp", ".old"} {
		if _, err := os.Lstat(store + suf); !os.IsNotExist(err) {
			t.Fatalf("%s still present: %v", store+suf, err)
		}
	}
}

func assertStoreCommitted(t *testing.T, e *Engine, want string) {
	t.Helper()
	store := e.cacheStorePath("default", "xc")
	assertNoCacheStaging(t, store)
	got, err := os.ReadFile(filepath.Join(store, "state.bin"))
	if want == "" {
		if err == nil {
			t.Fatalf("uncommitted clone was published: %q", got)
		}
		if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if _, err := os.Lstat(store); !os.IsNotExist(err) {
			t.Fatalf("uncommitted cache became the store: %v", err)
		}
		return
	}
	if err != nil || string(got) != want {
		t.Fatalf("store %q err=%v, want %q", got, err, want)
	}
}

func assertPodRestored(t *testing.T, e *Engine, uid, want string) {
	t.Helper()
	pod := cachePod("c", uid, map[string]string{"cache.darwin.node/xc": "/Users/mac/DD"})
	if _, _, err := e.prepareCaches(pod); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(e.cachePodDir(uid, "xc"), "state.bin"))
	if want == "" {
		if !os.IsNotExist(err) {
			t.Fatalf("pod cache poisoned with %q (%v)", got, err)
		}
		return
	}
	if err != nil || string(got) != want {
		t.Fatalf("pod cache %q err=%v, want %q", got, err, want)
	}
}

// Startup recovery: Engine.New runs Recover before the next pod is created.
func TestCacheStoreCrashWindowsHealedOnStartup(t *testing.T) {
	for _, tc := range cacheCrashCases() {
		t.Run(tc.name, func(t *testing.T) {
			cache := t.TempDir()
			plantCacheTree(t, cache, tc.tree)
			e := engineOnCache(t, cache)
			assertStoreCommitted(t, e, tc.want)
			assertPodRestored(t, e, "uid-startup", tc.want)
		})
	}
}

// Restore recovery: a torn store that appears after startup (failed commit
// in this process) is healed when the next pod restores, not only by New.
func TestCacheStoreCrashWindowsHealedOnRestore(t *testing.T) {
	for _, tc := range cacheCrashCases() {
		t.Run(tc.name, func(t *testing.T) {
			cache := t.TempDir()
			e := engineOnCache(t, cache)
			plantCacheTree(t, cache, tc.tree)
			assertPodRestored(t, e, "uid-restore", tc.want)
			assertStoreCommitted(t, e, tc.want)
		})
	}
}

// A healed store plus later guest writes must commit, and must not drop
// the bytes that were only under .old when the process died.
func TestSnapshotAfterCrashRecoveryCommitsAndCleans(t *testing.T) {
	cache := t.TempDir()
	plantCacheTree(t, cache, map[string]string{
		"xc.old/state.bin": "committed-snapshot",
		"xc.tmp/state.bin": "inflight-uncommitted",
	})
	e := engineOnCache(t, cache)
	pod := cachePod("snap", "uid-snap", map[string]string{"cache.darwin.node/xc": "/Users/mac/DD"})
	if _, _, err := e.prepareCaches(pod); err != nil {
		t.Fatal(err)
	}
	host := e.cachePodDir("uid-snap", "xc")
	if err := os.WriteFile(filepath.Join(host, "guest.bin"), []byte("from-guest"), 0o644); err != nil {
		t.Fatal(err)
	}
	e.snapshotPodCaches(&podRecord{pod: pod})

	store := e.cacheStorePath("default", "xc")
	assertNoCacheStaging(t, store)
	got, err := os.ReadFile(filepath.Join(store, "state.bin"))
	if err != nil || string(got) != "committed-snapshot" {
		t.Fatalf("committed bytes lost across snapshot: %q %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(store, "guest.bin"))
	if err != nil || string(got) != "from-guest" {
		t.Fatalf("guest bytes not committed: %q %v", got, err)
	}
}
