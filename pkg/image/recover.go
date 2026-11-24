package image

import (
	"os"
	"path/filepath"
)

// Recover heals a pull cache left by a crash between commitCacheDir's two
// renames. image-replacing/<key> is the last committed tree when images/<key>
// is missing. image-partial/<key> is an unfinished pull and is never
// published. A symlink is removed as a symlink; the target is left alone.
func Recover(cacheRoot string) error {
	if cacheRoot == "" {
		return nil
	}
	// Legacy keys are the old ":" → "--", "/" → "_" names. Two refs
	// shared one, so renaming a leftover onto the new hash would keep
	// that mix-up. Drop them. A hash-named tree is still restored below.
	if err := dropLegacyCacheKeys(cacheRoot); err != nil {
		return err
	}
	keys := map[string]struct{}{}
	var first error
	remember := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	for _, sub := range []string{"image-partial", "image-replacing"} {
		names, err := stagingNames(cacheRoot, sub)
		if err != nil {
			remember(err)
			continue
		}
		for _, name := range names {
			keys[name] = struct{}{}
		}
	}
	for key := range keys {
		remember(recoverImageKey(cacheRoot, key))
	}
	return first
}

// stagingNames lists one staging directory. ReadDir follows a symlink, so a
// staging path that is not a real directory is removed as itself and not
// listed. That keeps a symlink from selecting names outside the cache.
func stagingNames(cacheRoot, sub string) ([]string, error) {
	dir := filepath.Join(cacheRoot, sub)
	fi, err := os.Lstat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return nil, os.Remove(dir)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(ents))
	for _, ent := range ents {
		names = append(names, ent.Name())
	}
	return names, nil
}

func recoverImageKey(cacheRoot, key string) error {
	if err := safePathSegment(key); err != nil {
		// Not a cache key. Joining it on would be the escape hatch.
		return nil
	}
	final := filepath.Join(cacheRoot, "images", key)
	partial := filepath.Join(cacheRoot, "image-partial", key)
	backup := filepath.Join(cacheRoot, "image-replacing", key)

	finalDir, err := realDir(final)
	if err != nil {
		return err
	}
	if !finalDir {
		backupDir, berr := realDir(backup)
		if berr != nil {
			return berr
		}
		if backupDir {
			if _, lerr := LoadCommitted(backup); lerr == nil {
				if _, statErr := os.Lstat(final); statErr == nil {
					// A file or symlink already occupies the cache key.
					// Do not rename a directory onto it.
					if err := removeCacheTree(backup); err != nil {
						return err
					}
				} else if !os.IsNotExist(statErr) {
					return statErr
				} else {
					if err := os.MkdirAll(filepath.Dir(final), 0o700); err != nil {
						return err
					}
					if err := os.Rename(backup, final); err != nil {
						return err
					}
				}
			} else if err := removeCacheTree(backup); err != nil {
				return err
			}
		} else if err := removeCacheTree(backup); err != nil {
			return err
		}
	} else if err := removeCacheTree(backup); err != nil {
		return err
	}
	return removeCacheTree(partial)
}

func dropLegacyCacheKeys(cacheRoot string) error {
	for _, sub := range []string{"images", "image-partial", "image-replacing"} {
		dir := filepath.Join(cacheRoot, sub)
		fi, err := os.Lstat(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			continue
		}
		ents, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, ent := range ents {
			if validCacheKey(ent.Name()) {
				continue
			}
			if err := removeCacheTree(filepath.Join(dir, ent.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func realDir(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return false, nil
	}
	return fi.IsDir(), nil
}

func removeCacheTree(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return os.Remove(path)
	}
	return os.RemoveAll(path)
}
