package engine

import (
	"os"
	"path/filepath"
	"strings"
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

// Recover republishes committed cache stores, then runs the 24h pod-cache
// sweep. Called from New so production gets crash recovery without a
// cmd/darwin-node change. A store-recovery error does not skip the sweep.
func (e *Engine) Recover() error {
	if e == nil {
		return nil
	}
	errStores := recoverCacheStores(e.cfg.CacheDir)
	errPods := RecoverCache(e.cfg.CacheDir, e.slots, defaultPodCacheMaxAge)
	if errStores != nil {
		return errStores
	}
	return errPods
}

// recoverCacheStores heals every cache-store entry under cacheDir.
// Snapshot swaps are two renames (live to name.old, then name.tmp to live).
// A crash between them leaves the last committed tree only at name.old;
// a crash beside a live store leaves name.tmp and sometimes name.old as
// leftovers. name.tmp is never published: a half-copied clone and a
// finished clone are the same shape on disk.
func recoverCacheStores(cacheDir string) error {
	if cacheDir == "" {
		return nil
	}
	root := filepath.Join(cacheDir, cacheStoreDirName)
	nsEnts, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var first error
	remember := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for _, ns := range nsEnts {
		if !ns.IsDir() {
			continue
		}
		dir := filepath.Join(root, ns.Name())
		ents, err := os.ReadDir(dir)
		if err != nil {
			remember(err)
			continue
		}
		bases := map[string]struct{}{}
		for _, ent := range ents {
			n := ent.Name()
			switch {
			case strings.HasSuffix(n, ".tmp"):
				bases[strings.TrimSuffix(n, ".tmp")] = struct{}{}
			case strings.HasSuffix(n, ".old"):
				bases[strings.TrimSuffix(n, ".old")] = struct{}{}
			default:
				bases[n] = struct{}{}
			}
		}
		for base := range bases {
			if base == "" || base == "." || base == ".." {
				continue
			}
			remember(recoverCacheStore(filepath.Join(dir, base)))
		}
	}
	return first
}

// recoverCacheStore makes store the last committed snapshot.
// Store present: it wins; .tmp and .old are leftovers.
// Store missing and .old present: .old is the committed tree (crash
// between the two renames, or after .tmp was removed).
// Otherwise .tmp is an uncommitted clone and is removed.
func recoverCacheStore(store string) error {
	tmp := store + ".tmp"
	old := store + ".old"
	storeOK, err := pathExists(store)
	if err != nil {
		return err
	}
	tmpOK, err := pathExists(tmp)
	if err != nil {
		return err
	}
	oldOK, err := pathExists(old)
	if err != nil {
		return err
	}
	if storeOK {
		if tmpOK {
			if err := os.RemoveAll(tmp); err != nil {
				return err
			}
		}
		if oldOK {
			if err := os.RemoveAll(old); err != nil {
				return err
			}
		}
		return nil
	}
	if tmpOK {
		if err := os.RemoveAll(tmp); err != nil {
			return err
		}
	}
	if oldOK {
		if err := os.Rename(old, store); err != nil {
			return err
		}
	}
	return nil
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}
