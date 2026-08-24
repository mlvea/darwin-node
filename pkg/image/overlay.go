package image

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/darwin-node/darwin-node/internal/clonefile"
	"github.com/darwin-node/darwin-node/internal/digest"

	ocidigest "github.com/opencontainers/go-digest"
)

// LocalImage is a cached, verified image on disk.
type LocalImage struct {
	Dir      string
	Config   Config
	DiskPath string
	AuxPath  string
	DiskDig  ocidigest.Digest
	AuxDig   ocidigest.Digest
}

// Overlay is a per-pod CoW clone of disk+aux.
type Overlay struct {
	DiskPath string
	AuxPath  string
	Dir      string
}

// Verify checks sidecar digests when expected values are known.
func (i LocalImage) Verify() error {
	if i.DiskPath != "" {
		if err := digest.Verify(i.DiskPath, i.DiskDig); err != nil {
			return fmt.Errorf("disk: %w", err)
		}
	}
	if i.AuxPath != "" {
		if err := digest.Verify(i.AuxPath, i.AuxDig); err != nil {
			return fmt.Errorf("aux: %w", err)
		}
	}
	return nil
}

// CloneOverlay clonefile's disk and aux into destDir.
// Derived from Agoda macOS-vz-kubelet (Apache-2.0) CoW overlays;
// overlays live next to the pod, not in /tmp.
func CloneOverlay(img LocalImage, destDir string) (Overlay, error) {
	if err := os.MkdirAll(destDir, 0o700); err != nil {
		return Overlay{}, err
	}
	ov := Overlay{Dir: destDir}
	if img.DiskPath != "" {
		ov.DiskPath = filepath.Join(destDir, "disk.img")
		if err := clonefile.File(img.DiskPath, ov.DiskPath); err != nil {
			return Overlay{}, fmt.Errorf("clone disk: %w", err)
		}
	}
	if img.AuxPath != "" {
		ov.AuxPath = filepath.Join(destDir, "aux.img")
		if err := clonefile.File(img.AuxPath, ov.AuxPath); err != nil {
			return Overlay{}, fmt.Errorf("clone aux: %w", err)
		}
	}
	return ov, nil
}

// Remove deletes the overlay directory.
func (o Overlay) Remove() error {
	if o.Dir == "" {
		return nil
	}
	return os.RemoveAll(o.Dir)
}
