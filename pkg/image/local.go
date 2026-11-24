package image

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/darwin-node/darwin-node/internal/digest"

	ocidigest "github.com/opencontainers/go-digest"
)

// LoadDir loads a baked image directory (config.json + disk.img + aux.img).
func LoadDir(dir string) (LocalImage, error) {
	if err := rejectImageSymlinks(dir); err != nil {
		return LocalImage{}, err
	}
	cfgPath := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return LocalImage{}, fmt.Errorf("read config.json: %w", err)
	}
	cfg, err := ParseConfig(raw)
	if err != nil {
		return LocalImage{}, err
	}
	img := LocalImage{Dir: dir, Config: cfg}
	for _, s := range cfg.Storage {
		if s.File != "" {
			if err := safePathSegment(s.File); err != nil {
				return LocalImage{}, fmt.Errorf("image config file: %w", err)
			}
		}
		p := filepath.Join(dir, s.File)
		switch CanonicalMediaType(s.MediaType) {
		case MediaTypeDisk:
			img.DiskPath = p
		case MediaTypeAux:
			img.AuxPath = p
		}
	}
	if img.DiskPath == "" {
		img.DiskPath = filepath.Join(dir, "disk.img")
	}
	if img.AuxPath == "" {
		img.AuxPath = filepath.Join(dir, "aux.img")
	}
	if _, err := os.Stat(img.DiskPath); err != nil {
		return LocalImage{}, fmt.Errorf("disk.img: %w", err)
	}
	if _, err := os.Stat(img.AuxPath); err != nil {
		return LocalImage{}, fmt.Errorf("aux.img: %w", err)
	}
	prov, err := LoadProvenance(dir)
	if err != nil && !os.IsNotExist(err) {
		return LocalImage{}, err
	}
	if err == nil {
		img.applyProvenance(prov)
	}
	return img, nil
}

// CacheKey is the hex sha256 of the full reference, one path segment.
// Replacing ":" and "/" collided "team/macos:15" with "team_macos:15",
// so the first pull decided what the other reference booted. Old
// directories are not renamed onto the new key: that would keep the
// collision. Recover deletes keys that are not this hash.
func CacheKey(ref string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(ref)))
	return hex.EncodeToString(sum[:])
}

// validCacheKey reports a directory name produced by CacheKey.
func validCacheKey(name string) bool {
	if len(name) != sha256.Size*2 {
		return false
	}
	for _, c := range name {
		switch {
		case c >= '0' && c <= '9', c >= 'a' && c <= 'f':
		default:
			return false
		}
	}
	return true
}

// rejectImageSymlinks refuses a symlink directory and any symlink inside
// it. os.Stat and clonefile follow those links, so a disk.img link to
// another pod's overlay would be cloned into this pod.
func rejectImageSymlinks(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("image directory is a symlink")
	}
	if !fi.IsDir() {
		return fmt.Errorf("image path is not a directory")
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, ent := range ents {
		info, err := ent.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("image file %q is a symlink", ent.Name())
		}
	}
	return nil
}

// CacheDir is cacheRoot/images/<CacheKey(ref)>.
func CacheDir(cacheRoot, ref string) string {
	return filepath.Join(cacheRoot, "images", CacheKey(ref))
}

// LoadCommitted loads a finished pull. provenance.json is written only after
// disk.img and aux.img are in place, so a directory without it is an
// unfinished pull (a crash or a short write) and must not be booted.
// Local baked directories use LoadDir; they are not required to carry this
// file.
func LoadCommitted(dir string) (LocalImage, error) {
	if _, err := os.Stat(filepath.Join(dir, ProvenanceFile)); err != nil {
		return LocalImage{}, fmt.Errorf("pull cache %s: %w", ProvenanceFile, err)
	}
	img, err := LoadDir(dir)
	if err != nil {
		return LocalImage{}, err
	}
	if err := img.VerifyOptional(); err != nil {
		return LocalImage{}, err
	}
	return img, nil
}

// VerifyOptional checks disk/aux against pull-time provenance when present.
// Locally baked images without provenance or sidecars skip verification.
func (i LocalImage) VerifyOptional() error {
	if err := verifyOptional(i.DiskPath, i.DiskDig); err != nil {
		return err
	}
	if err := verifyOptional(i.AuxPath, i.AuxDig); err != nil {
		return err
	}
	return nil
}

func verifyOptional(path string, expected ocidigest.Digest) error {
	if path == "" {
		return nil
	}
	if expected != "" {
		return digest.Verify(path, expected)
	}
	if _, err := os.Stat(path + digest.Suffix); err == nil {
		return digest.Verify(path, "")
	}
	return nil
}
