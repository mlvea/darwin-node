// Pod cache volumes: annotation-declared guest directories backed by a
// host-side CoW store. Restore is an APFS clonefile into the pod dir.
// Consumers get a read-only virtio-fs share. Write-back is opt-in
// (cache-write.darwin.node/<name>: "true") and only runs when the pod's
// namespace or ServiceAccount is listed in --cache-writers, and only after
// the workload exits 0. The commit clones into <store>.tmp and renames it
// into place. A failed or interrupted run does not replace the store.
// No new guest protocol: everything rides existing shares and Materialize.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/mlvea/darwin-node/internal/clonefile"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/types"
	"github.com/mlvea/darwin-node/pkg/volume"

	corev1 "k8s.io/api/core/v1"
)

const cacheStoreDirName = "cache-store"

// cacheWriteAnnotationPrefix is types.AnnotationCacheWritePrefix. Kept as a
// local name so the parser reads like the cache declaration prefix.
const cacheWriteAnnotationPrefix = types.AnnotationCacheWritePrefix

// CacheVol is one declared cache: annotation cache.darwin.node/<name> with
// an absolute guest path as its value.
type CacheVol struct {
	Name      string
	GuestPath string
}

// ParseCacheAnnotations extracts and validates declared caches.
func ParseCacheAnnotations(pod *corev1.Pod) ([]CacheVol, error) {
	if pod == nil {
		return nil, nil
	}
	var keys []string
	for k := range pod.Annotations {
		if strings.HasPrefix(k, types.AnnotationCachePrefix) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	sort.Strings(keys)
	out := make([]CacheVol, 0, len(keys))
	seenPaths := map[string]string{}
	for _, k := range keys {
		name := strings.TrimPrefix(k, types.AnnotationCachePrefix)
		if name == "" || !validCacheName(name) {
			return nil, fmt.Errorf("cache annotation %q: name must match [A-Za-z0-9._-]+", k)
		}
		// .tmp and .old are the snapshot staging directories beside the
		// store. A cache with that suffix would alias a crash leftover.
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".old") {
			return nil, fmt.Errorf("cache annotation %q: name must not end in .tmp or .old", k)
		}
		guest, err := cleanGuestPath(pod.Annotations[k])
		if err != nil {
			return nil, fmt.Errorf("cache annotation %q: %w", k, err)
		}
		if prev, dup := seenPaths[guest]; dup {
			return nil, fmt.Errorf("cache annotations %q and %q declare the same guest path %q", prev, k, guest)
		}
		seenPaths[guest] = k
		out = append(out, CacheVol{Name: name, GuestPath: guest})
	}
	return out, nil
}

// cacheWriteSet returns the declared caches this pod asked to write back.
// A write annotation that names no declared cache, or whose value is not
// true or false, is rejected.
func cacheWriteSet(pod *corev1.Pod, declared []CacheVol) (map[string]bool, error) {
	known := map[string]bool{}
	for _, c := range declared {
		known[c.Name] = true
	}
	if pod == nil {
		return map[string]bool{}, nil
	}
	var keys []string
	for k := range pod.Annotations {
		if strings.HasPrefix(k, cacheWriteAnnotationPrefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	out := map[string]bool{}
	for _, k := range keys {
		name := strings.TrimPrefix(k, cacheWriteAnnotationPrefix)
		if name == "" || !validCacheName(name) || strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".old") {
			return nil, fmt.Errorf("cache write annotation %q: name must match [A-Za-z0-9._-]+ and must not end in .tmp or .old", k)
		}
		switch strings.TrimSpace(pod.Annotations[k]) {
		case "true":
			if !known[name] {
				return nil, fmt.Errorf("cache write annotation %q: cache.darwin.node/%s is not declared", k, name)
			}
			out[name] = true
		case "false":
		default:
			return nil, fmt.Errorf("cache write annotation %q: value must be true or false", k)
		}
	}
	return out, nil
}

// podMayWriteCache reports whether --cache-writers names this pod's
// namespace, or that namespace and its ServiceAccount. An empty
// ServiceAccountName is "default", matching Kubernetes.
func (e *Engine) podMayWriteCache(pod *corev1.Pod) bool {
	if e == nil || pod == nil {
		return false
	}
	ns := pod.Namespace
	sa := pod.Spec.ServiceAccountName
	if sa == "" {
		sa = "default"
	}
	for _, raw := range e.cfg.CacheWriters {
		entry := strings.TrimSpace(raw)
		if entry == "" {
			continue
		}
		if entry == ns || entry == ns+"/"+sa {
			return true
		}
	}
	return false
}

func validCacheName(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// cleanGuestPath validates an annotation value as an absolute guest path and
// rejects any ".." element, so a cache can never escape its mount point.
func cleanGuestPath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("value must be an absolute guest path")
	}
	for _, seg := range strings.Split(filepath.ToSlash(raw), "/") {
		if seg == ".." {
			return "", fmt.Errorf("path traversal is not allowed")
		}
	}
	cleaned := filepath.Clean(raw)
	if !filepath.IsAbs(cleaned) || cleaned == "/" {
		return "", fmt.Errorf("value must be an absolute guest path below /")
	}
	return cleaned, nil
}

func cacheShareName(name string) string { return "cache.darwin.node." + name }

func (e *Engine) cachePodDir(uid, name string) string {
	return filepath.Join(e.cfg.CacheDir, "pods", uid, "caches", name)
}

func (e *Engine) cacheStorePath(ns, name string) string {
	return filepath.Join(e.cfg.CacheDir, cacheStoreDirName, ns, name)
}

// cloneCacheTree is cloneCacheDir. Tests replace it so two snapshots of
// one store can be forced to overlap.
var cloneCacheTree = cloneCacheDir

// cloneCacheDir restores a cache tree. On Darwin, prefer a single-shot
// clonefile(2) of the directory (APFS CoW); fall back to walk+per-file
// clone/copy when that fails (non-APFS volumes). Elsewhere always use Dir
// so Linux never hits copy_file_range on a directory.
func cloneCacheDir(src, dst string) error {
	if runtime.GOOS == "darwin" {
		if err := clonefile.File(src, dst); err == nil {
			return nil
		}
	}
	return clonefile.Dir(src, dst)
}

// prepareCaches restores each declared cache from the store (CoW clone when
// present, empty dir otherwise) and returns the virtio-fs shares and link
// placements that expose them at the annotated guest paths.
func (e *Engine) prepareCaches(pod *corev1.Pod) ([]types.Share, []volume.Placement, error) {
	e.cacheSnapMu.Lock()
	defer e.cacheSnapMu.Unlock()
	caches, err := ParseCacheAnnotations(pod)
	if err != nil || len(caches) == 0 {
		return nil, nil, err
	}
	uid := string(pod.UID)
	ns := pod.Namespace
	if err := safeSegment("pod uid", uid); err != nil {
		return nil, nil, err
	}
	if err := safeSegment("namespace", ns); err != nil {
		return nil, nil, err
	}
	writes, err := cacheWriteSet(pod, caches)
	if err != nil {
		return nil, nil, err
	}
	mayWrite := e.podMayWriteCache(pod)
	saName := pod.Spec.ServiceAccountName
	if saName == "" {
		saName = "default"
	}
	var shares []types.Share
	var places []volume.Placement
	for _, c := range caches {
		hostDir := e.cachePodDir(uid, c.Name)
		store := e.cacheStorePath(ns, c.Name)
		if err := recoverCacheStore(store); err != nil {
			return nil, nil, fmt.Errorf("cache %q recover: %w", c.Name, err)
		}
		if err := os.RemoveAll(hostDir); err != nil {
			return nil, nil, fmt.Errorf("cache %q: %w", c.Name, err)
		}
		if _, err := os.Stat(store); err == nil {
			if err := cloneCacheTree(store, hostDir); err != nil {
				return nil, nil, fmt.Errorf("cache %q restore: %w", c.Name, err)
			}
			e.events.Normal(context.Background(), event.ReasonCacheRestored, c.Name+" -> "+c.GuestPath)
		} else if err := os.MkdirAll(hostDir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("cache %q: %w", c.Name, err)
		}
		writable := mayWrite && writes[c.Name]
		if writes[c.Name] && !mayWrite {
			e.events.Warn(context.Background(), event.ReasonCacheReadOnly,
				fmt.Sprintf("cache %s stays read-only: %s/%s is not in --cache-writers", c.Name, ns, saName))
		}
		shares = append(shares, types.Share{
			Name:     cacheShareName(c.Name),
			HostPath: hostDir,
			ReadOnly: !writable,
		})
		places = append(places, volume.Placement{
			Name:      cacheShareName(c.Name),
			HostPath:  hostDir,
			GuestPath: c.GuestPath,
			ReadOnly:  !writable,
			Mode:      "link",
		})
	}
	return shares, places, nil
}

// snapshotPodCaches clones each writable cache out of the pod dir into the
// node store. It commits only when the workload exited 0, the pod was not
// killed, and the operator allowlist plus the write annotation both say
// this pod may replace that cache. Best-effort: failures are logged and
// never fail the caller. The commit writes a new directory and renames it
// over the live store.
func (e *Engine) snapshotPodCaches(rec *podRecord) {
	if rec == nil {
		return
	}
	pod, ok := cacheSnapshotPod(rec)
	if !ok || !e.podMayWriteCache(pod) {
		return
	}
	e.cacheSnapMu.Lock()
	defer e.cacheSnapMu.Unlock()
	caches, err := ParseCacheAnnotations(pod)
	if err != nil || len(caches) == 0 {
		return
	}
	writes, err := cacheWriteSet(pod, caches)
	if err != nil {
		e.events.Warn(context.Background(), event.ReasonCacheSaved, err.Error())
		return
	}
	if len(writes) == 0 {
		return
	}
	uid := string(pod.UID)
	if err := safeSegment("namespace", pod.Namespace); err != nil {
		e.events.Warn(context.Background(), event.ReasonCacheSaved, err.Error())
		return
	}
	for _, c := range caches {
		if !writes[c.Name] {
			continue
		}
		src := e.cachePodDir(uid, c.Name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		store := e.cacheStorePath(pod.Namespace, c.Name)
		// Heal a previous crash before mutating. Otherwise RemoveAll of
		// .old can delete the only committed tree.
		if err := recoverCacheStore(store); err != nil {
			e.events.Warn(context.Background(), event.ReasonCacheSaved, fmt.Sprintf("cache %q recover: %v", c.Name, err))
			continue
		}
		tmp := store + ".tmp"
		old := store + ".old"
		if err := os.RemoveAll(tmp); err != nil {
			e.events.Warn(context.Background(), event.ReasonCacheSaved, fmt.Sprintf("cache %q: %v", c.Name, err))
			continue
		}
		if err := cloneCacheTree(src, tmp); err != nil {
			_ = os.RemoveAll(tmp)
			e.events.Warn(context.Background(), event.ReasonCacheSaved, fmt.Sprintf("cache %q clone: %v", c.Name, err))
			continue
		}
		// Commit by renaming the live store aside, then the new tree into
		// place. .old is removed only after that rename succeeds, and only
		// a store that was present is moved aside — a missing store means
		// .old may be the last committed copy.
		live, err := pathExists(store)
		if err != nil {
			e.events.Warn(context.Background(), event.ReasonCacheSaved, fmt.Sprintf("cache %q: %v", c.Name, err))
			continue
		}
		if live {
			if err := os.RemoveAll(old); err != nil {
				e.events.Warn(context.Background(), event.ReasonCacheSaved, fmt.Sprintf("cache %q: %v", c.Name, err))
				continue
			}
			if err := os.Rename(store, old); err != nil && !os.IsNotExist(err) {
				e.events.Warn(context.Background(), event.ReasonCacheSaved, fmt.Sprintf("cache %q: %v", c.Name, err))
				continue
			}
		}
		if err := os.Rename(tmp, store); err != nil {
			e.events.Warn(context.Background(), event.ReasonCacheSaved, fmt.Sprintf("cache %q swap: %v", c.Name, err))
			if live {
				_ = os.Rename(old, store)
			}
			continue
		}
		_ = os.RemoveAll(old)
		e.events.Normal(context.Background(), event.ReasonCacheSaved, c.GuestPath+" -> "+store)
	}
}

// cacheSnapshotPod is the pod whose cache may be committed. The workload
// must have exited 0 and must not have been killed or already failed.
// The returned pod is a copy, so a later annotation edit cannot change
// which caches this commit writes.
func cacheSnapshotPod(rec *podRecord) (*corev1.Pod, bool) {
	if rec == nil {
		return nil, false
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.pod == nil || rec.killedCode != nil || rec.guestExit == nil || *rec.guestExit != 0 {
		return nil, false
	}
	if rec.phase == corev1.PodFailed {
		return nil, false
	}
	return rec.pod.DeepCopy(), true
}
