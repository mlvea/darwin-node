// Pod cache volumes: annotation-declared guest directories backed by a
// host-side CoW store. Restore is an APFS clonefile into the pod dir; the
// directory is shared read-write over virtio-fs (link placement), so guest
// writes land on host disk and a graceful Delete re-snapshots them. No new
// guest protocol: everything rides existing shares and Materialize.
package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/darwin-node/darwin-node/internal/clonefile"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/types"
	"github.com/darwin-node/darwin-node/pkg/volume"

	corev1 "k8s.io/api/core/v1"
)

const cacheStoreDirName = "cache-store"

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

// prepareCaches restores each declared cache from the store (CoW clone when
// present, empty dir otherwise) and returns the virtio-fs shares and link
// placements that expose them at the annotated guest paths.
func (e *Engine) prepareCaches(pod *corev1.Pod) ([]types.Share, []volume.Placement, error) {
	caches, err := ParseCacheAnnotations(pod)
	if err != nil || len(caches) == 0 {
		return nil, nil, err
	}
	uid := string(pod.UID)
	ns := pod.Namespace
	var shares []types.Share
	var places []volume.Placement
	for _, c := range caches {
		hostDir := e.cachePodDir(uid, c.Name)
		store := e.cacheStorePath(ns, c.Name)
		if err := os.RemoveAll(hostDir); err != nil {
			return nil, nil, fmt.Errorf("cache %q: %w", c.Name, err)
		}
		if _, err := os.Stat(store); err == nil {
			if err := clonefile.File(store, hostDir); err != nil {
				return nil, nil, fmt.Errorf("cache %q restore: %w", c.Name, err)
			}
			e.events.Normal(context.Background(), event.ReasonCacheRestored, c.Name+" -> "+c.GuestPath)
		} else if err := os.MkdirAll(hostDir, 0o755); err != nil {
			return nil, nil, fmt.Errorf("cache %q: %w", c.Name, err)
		}
		shares = append(shares, types.Share{Name: cacheShareName(c.Name), HostPath: hostDir})
		places = append(places, volume.Placement{
			Name:      cacheShareName(c.Name),
			HostPath:  hostDir,
			GuestPath: c.GuestPath,
			Mode:      "link",
		})
	}
	return shares, places, nil
}

// snapshotPodCaches clones each declared cache out of the pod dir into the
// node store. Best-effort: failures are logged and never fail the delete.
func (e *Engine) snapshotPodCaches(rec *podRecord) {
	pod := rec.pod
	if pod == nil {
		return
	}
	caches, err := ParseCacheAnnotations(pod)
	if err != nil || len(caches) == 0 {
		return
	}
	uid := string(pod.UID)
	for _, c := range caches {
		src := e.cachePodDir(uid, c.Name)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		store := e.cacheStorePath(pod.Namespace, c.Name)
		tmp := store + ".tmp"
		if err := os.RemoveAll(tmp); err != nil {
			e.events.Warn(context.Background(), event.ReasonCacheSaved, fmt.Sprintf("cache %q: %v", c.Name, err))
			continue
		}
		if err := clonefile.File(src, tmp); err != nil {
			e.events.Warn(context.Background(), event.ReasonCacheSaved, fmt.Sprintf("cache %q clone: %v", c.Name, err))
			continue
		}
		// Swap with a recovery step: a crash between the two renames leaves
		// the previous store intact under .old rather than destroyed.
		_ = os.RemoveAll(store + ".old")
		if err := os.Rename(store, store+".old"); err != nil && !os.IsNotExist(err) {
			e.events.Warn(context.Background(), event.ReasonCacheSaved, fmt.Sprintf("cache %q: %v", c.Name, err))
			continue
		}
		if err := os.Rename(tmp, store); err != nil {
			e.events.Warn(context.Background(), event.ReasonCacheSaved, fmt.Sprintf("cache %q swap: %v", c.Name, err))
			// Best-effort rollback so the old snapshot stays live.
			_ = os.Rename(store+".old", store)
			continue
		}
		_ = os.RemoveAll(store + ".old")
		e.events.Normal(context.Background(), event.ReasonCacheSaved, c.GuestPath+" -> "+store)
	}
}
