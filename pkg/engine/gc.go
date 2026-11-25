package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/image"

	"github.com/shirou/gopsutil/v4/disk"
	corev1 "k8s.io/api/core/v1"
)

// gcItem is one image directory or cache store GC may delete.
type gcItem struct {
	id      string
	path    string
	mod     time.Time
	baseKey string // image cache key this item was built from
	kind    string // "image" or "cache"
	name    string // reference or namespace/name, for the event
}

// StartDiskGC evaluates image and cache-store GC once, then on
// ImageGCInterval, until ctx is cancelled. ImageGC false does nothing.
// The loop does not block startup.
func (e *Engine) StartDiskGC(ctx context.Context) {
	if e == nil || !e.cfg.ImageGC {
		return
	}
	interval := e.cfg.ImageGCInterval
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	go func() {
		e.runDiskGC(ctx)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.runDiskGC(ctx)
			}
		}
	}()
}

func (e *Engine) runDiskGC(ctx context.Context) {
	if err := e.GarbageCollect(ctx); err != nil && ctx.Err() == nil {
		slog.Warn("disk gc", "err", err)
	}
}

// GarbageCollect deletes unused images and cache stores while the cache
// filesystem is at or above the high threshold, oldest first, until usage
// is at or below the low threshold. An image used by a pending or running
// pod, an image in the warm pool (including the image the pool is configured
// to boot), and a delta base that a remaining image still names are kept.
// Each deletion emits ImageEvicted or CacheEvicted.
func (e *Engine) GarbageCollect(ctx context.Context) error {
	return e.collectGarbage(ctx, nil, nil)
}

func (e *Engine) collectGarbage(ctx context.Context, used func(string) (float64, error), remove func(string) error) error {
	if e == nil || !e.cfg.ImageGC {
		return nil
	}
	root := e.cfg.CacheDir
	if strings.TrimSpace(root) == "" {
		return nil
	}
	if used == nil {
		used = cacheDiskUsed
	}
	if remove == nil {
		remove = func(path string) error { return removeWithin(root, path) }
	}
	high := float64(e.cfg.ImageGCHigh) / 100
	low := float64(e.cfg.ImageGCLow) / 100
	u, err := used(root)
	if err != nil {
		return err
	}
	if u < high {
		return nil
	}
	failed := map[string]struct{}{}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		u, err = used(root)
		if err != nil {
			return err
		}
		if u <= low {
			return nil
		}
		items, err := e.listGCItems()
		if err != nil {
			return err
		}
		victim, ok := nextGCVictim(items, e.gcPins(), failed)
		if !ok {
			return nil
		}
		if err := remove(victim.path); err != nil {
			failed[victim.id] = struct{}{}
			e.events.Warn(ctx, victim.reason(), fmt.Sprintf("evict %s: %v", victim.name, err))
			continue
		}
		e.events.Normal(ctx, victim.reason(), "evicted "+victim.kind+" "+victim.name)
	}
}

func (it gcItem) reason() string {
	if it.kind == "cache" {
		return event.ReasonCacheEvicted
	}
	return event.ReasonImageEvicted
}

// gcPins are cache keys and cache-store ids that must not be deleted.
// Pending and running pods pin their image and their declared cache stores.
// Succeeded and failed pods do not. Warm-pool VMs pin their image, and so
// does the configured warm image while WarmSlots is non-zero.
func (e *Engine) gcPins() map[string]struct{} {
	pins := map[string]struct{}{}
	e.mu.RLock()
	for _, rec := range e.pods {
		rec.mu.Lock()
		pod := rec.pod
		phase := rec.phase
		rec.mu.Unlock()
		if pod == nil || (phase != corev1.PodPending && phase != corev1.PodRunning) {
			continue
		}
		if len(pod.Spec.Containers) > 0 {
			if ref := strings.TrimSpace(pod.Spec.Containers[0].Image); ref != "" {
				pins[image.CacheKey(ref)] = struct{}{}
			}
		}
		vols, err := ParseCacheAnnotations(pod)
		if err != nil {
			continue
		}
		for _, v := range vols {
			pins[cacheGCID(pod.Namespace, v.Name)] = struct{}{}
		}
	}
	e.mu.RUnlock()
	for _, w := range e.warm.snapshot() {
		if ref := strings.TrimSpace(w.imgRef); ref != "" {
			pins[image.CacheKey(ref)] = struct{}{}
		}
	}
	if e.cfg.WarmSlots > 0 {
		if ref := strings.TrimSpace(e.warmImageRef()); ref != "" {
			pins[image.CacheKey(ref)] = struct{}{}
		}
	}
	return pins
}

func cacheGCID(ns, name string) string { return "cache:" + ns + "/" + name }

func (e *Engine) listGCItems() ([]gcItem, error) {
	images, err := image.ListCached(e.cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	items := make([]gcItem, 0, len(images))
	for _, img := range images {
		name := img.Ref
		if name == "" {
			name = img.Key
		}
		base := ""
		if img.BaseRef != "" {
			base = image.CacheKey(img.BaseRef)
		}
		items = append(items, gcItem{
			id: img.Key, path: img.Dir, mod: img.ModTime,
			baseKey: base, kind: "image", name: name,
		})
	}
	stores, err := listCacheStores(e.cfg.CacheDir)
	if err != nil {
		return nil, err
	}
	return append(items, stores...), nil
}

func listCacheStores(cacheRoot string) ([]gcItem, error) {
	root := filepath.Join(cacheRoot, cacheStoreDirName)
	nsEnts, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []gcItem
	for _, nsEnt := range nsEnts {
		nsPath := filepath.Join(root, nsEnt.Name())
		nsInfo, err := os.Lstat(nsPath)
		if err != nil || nsInfo.Mode()&os.ModeSymlink != 0 || !nsInfo.IsDir() {
			continue
		}
		names, err := os.ReadDir(nsPath)
		if err != nil {
			return nil, err
		}
		for _, nameEnt := range names {
			name := nameEnt.Name()
			if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".old") {
				continue
			}
			path := filepath.Join(nsPath, name)
			info, err := os.Lstat(path)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				continue
			}
			out = append(out, gcItem{
				id:   cacheGCID(nsEnt.Name(), name),
				path: path,
				mod:  info.ModTime(),
				kind: "cache",
				name: nsEnt.Name() + "/" + name,
			})
		}
	}
	return out, nil
}

// nextGCVictim is the oldest item that is not pinned and is not the base
// of an image that is still on disk. A self-reference does not pin the
// image. failed ids are skipped for this pass so a delete error cannot loop.
func nextGCVictim(items []gcItem, pins, failed map[string]struct{}) (gcItem, bool) {
	refs := map[string]struct{}{}
	for _, it := range items {
		if it.baseKey != "" && it.baseKey != it.id {
			refs[it.baseKey] = struct{}{}
		}
	}
	sort.Slice(items, func(i, j int) bool {
		if !items[i].mod.Equal(items[j].mod) {
			return items[i].mod.Before(items[j].mod)
		}
		return items[i].id < items[j].id
	})
	for _, it := range items {
		if _, ok := pins[it.id]; ok {
			continue
		}
		if _, ok := refs[it.id]; ok {
			continue
		}
		if _, ok := failed[it.id]; ok {
			continue
		}
		return it, true
	}
	return gcItem{}, false
}

func cacheDiskUsed(path string) (float64, error) {
	d, err := disk.Usage(path)
	if err != nil {
		return 0, fmt.Errorf("disk usage %s: %w", path, err)
	}
	if d.Total == 0 {
		return 0, nil
	}
	return float64(d.Used) / float64(d.Total), nil
}

// removeWithin deletes a real directory under root. A symlink is refused
// so GC cannot follow a link out of the cache and delete a host path.
func removeWithin(root, path string) error {
	root = filepath.Clean(root)
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("refuse to delete %s outside %s", path, root)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refuse to delete symlink %s", path)
	}
	if !info.IsDir() {
		return fmt.Errorf("refuse to delete non-directory %s", path)
	}
	return os.RemoveAll(path)
}
