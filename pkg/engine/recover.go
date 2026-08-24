package engine

import (
	"os"
	"path/filepath"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
)

const defaultPodCacheMaxAge = 24 * time.Hour

// RecoverCache deletes cache/pods/<uid> directories older than maxAge that are
// not held in slots. A zero or negative maxAge defaults to 24h.
//
// Remaining younger directories are not Rebuild'd into the slot table: a crash
// kills the VMs, so occupying slots for on-disk orphans would pin capacity.
func RecoverCache(cacheDir string, slots *capacity.Slots, maxAge time.Duration) error {
	if cacheDir == "" {
		return nil
	}
	if maxAge <= 0 {
		maxAge = defaultPodCacheMaxAge
	}
	podsDir := filepath.Join(cacheDir, "pods")
	entries, err := os.ReadDir(podsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	now := time.Now()
	for _, ent := range entries {
		if !ent.IsDir() {
			continue
		}
		uid := ent.Name()
		if slots != nil && slots.Holds(uid) {
			continue
		}
		info, err := ent.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) <= maxAge {
			continue
		}
		if err := os.RemoveAll(filepath.Join(podsDir, uid)); err != nil {
			return err
		}
	}
	return nil
}

// Recover runs the 24h pod-cache sweep. Called from New so production gets
// crash recovery without a cmd/darwin-node change.
func (e *Engine) Recover() error {
	if e == nil {
		return nil
	}
	return RecoverCache(e.cfg.CacheDir, e.slots, defaultPodCacheMaxAge)
}
