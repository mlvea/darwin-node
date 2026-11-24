package image

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RefFile is the image reference written next to a committed pull.
// The cache directory name is a hash, so GC reads this to name an eviction.
const RefFile = "image.ref"

// Cached is one committed image directory under cache/images.
type Cached struct {
	Key     string
	Dir     string
	Ref     string
	BaseRef string
	ModTime time.Time
}

// ListCached returns committed image directories. Symlinks are skipped
// so a link to another pod's disk is not treated as a cache entry.
// A missing images directory is an empty list.
func ListCached(cacheRoot string) ([]Cached, error) {
	root := filepath.Join(cacheRoot, "images")
	ents, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Cached
	for _, ent := range ents {
		name := ent.Name()
		if !validCacheKey(name) {
			continue
		}
		dir := filepath.Join(root, name)
		info, err := os.Lstat(dir)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			continue
		}
		out = append(out, Cached{
			Key:     name,
			Dir:     dir,
			Ref:     readImageRef(dir),
			BaseRef: readBaseRef(dir),
			ModTime: info.ModTime(),
		})
	}
	return out, nil
}

func writeImageRef(dir, ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(dir, RefFile), []byte(ref+"\n"), 0o600)
}

func readImageRef(dir string) string {
	p := filepath.Join(dir, RefFile)
	info, err := os.Lstat(p)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// readBaseRef is the immediate delta base, when this directory still
// records one. A symlink delta.json is ignored.
func readBaseRef(dir string) string {
	p := filepath.Join(dir, "delta.json")
	info, err := os.Lstat(p)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ""
	}
	man, err := ReadDeltaManifest(dir)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(man.BaseImage)
}
