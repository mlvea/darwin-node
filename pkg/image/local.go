package image

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/darwin-node/darwin-node/internal/digest"

	ocidigest "github.com/opencontainers/go-digest"
)

// LoadDir loads a baked image directory (config.json + disk.img + aux.img).
func LoadDir(dir string) (LocalImage, error) {
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

// CacheKey turns an image reference into a single path segment.
// Unlike Agoda's convertToPath (which splits on every ':'), a registry
// host:port stays one directory: "127.0.0.1:5000/macos:latest"
// → "127.0.0.1--5000_macos--latest".
func CacheKey(ref string) string {
	ref = strings.TrimSpace(ref)
	ref = strings.ReplaceAll(ref, ":", "--")
	ref = strings.ReplaceAll(ref, "/", "_")
	return ref
}

// CacheDir is cacheRoot/images/<CacheKey(ref)>.
func CacheDir(cacheRoot, ref string) string {
	return filepath.Join(cacheRoot, "images", CacheKey(ref))
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
