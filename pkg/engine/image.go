package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mlvea/darwin-node/pkg/image"

	corev1 "k8s.io/api/core/v1"
)

// resolveImage loads a local baked directory or a committed pull-cache
// entry, then clonefile's disk+aux into the pod overlay dir.
func (e *Engine) resolveImage(ctx context.Context, ref, uid string, pullPolicy corev1.PullPolicy, creds Credentials) (disk, aux, hardware string, err error) {
	if hostImagePath(ref) {
		if e.cfg.Cluster {
			return "", "", "", fmt.Errorf("load image %q: local image paths are not allowed in cluster mode", ref)
		}
		img, lerr := loadBakedDir(ref)
		if lerr != nil {
			return "", "", "", fmt.Errorf("load image %q: %w", ref, lerr)
		}
		return cloneResolved(e.cfg.CacheDir, img, uid)
	}
	img, err := e.resolveRegistryImage(ctx, ref, pullPolicy, creds.Pull)
	if err != nil {
		return "", "", "", fmt.Errorf("load image %q: %w", ref, err)
	}
	return cloneResolved(e.cfg.CacheDir, img, uid)
}

func loadBakedDir(dir string) (image.LocalImage, error) {
	img, err := image.LoadDir(dir)
	if err != nil {
		return image.LocalImage{}, err
	}
	if err := img.VerifyOptional(); err != nil {
		return image.LocalImage{}, err
	}
	return img, nil
}

func cloneResolved(cacheDir string, img image.LocalImage, uid string) (disk, aux, hardware string, err error) {
	ov, err := image.CloneOverlay(img, filepath.Join(cacheDir, "pods", uid, "overlay"))
	if err != nil {
		return "", "", "", err
	}
	return ov.DiskPath, ov.AuxPath, img.Config.HardwareModelData, nil
}

// hostImagePath is a host directory, an absolute path, or a relative path
// starting with '.'. Registry references are none of those unless a
// directory with that name exists in the process working directory.
func hostImagePath(ref string) bool {
	if ref == "" {
		return false
	}
	if filepath.IsAbs(ref) || strings.HasPrefix(ref, ".") {
		return true
	}
	fi, err := os.Lstat(ref)
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return true
	}
	return fi.IsDir()
}

// imageAuthorizer is the registry check a cached image requires.
// *image.Manager implements it. A test puller that does not is asked to Pull.
type imageAuthorizer interface {
	Authorize(ctx context.Context, ref string, creds image.RegistryCreds) error
}

func (e *Engine) resolveRegistryImage(ctx context.Context, ref string, pullPolicy corev1.PullPolicy, creds image.RegistryCreds) (image.LocalImage, error) {
	switch pullPolicy {
	case corev1.PullAlways:
		// Always re-checks the registry. A cached tag is not fresh.
		if e.puller == nil {
			return image.LocalImage{}, fmt.Errorf("image pull is not configured")
		}
		return e.puller.Pull(ctx, ref, creds, true)
	case corev1.PullNever:
		// Pull cache only. A local bake is hostImagePath above and may
		// omit provenance. An unfinished pull has the same files without
		// the lockfile; LoadDir plus VerifyOptional would boot it.
		img, err := image.LoadCommitted(image.CacheDir(e.cfg.CacheDir, ref))
		if err != nil {
			return image.LocalImage{}, err
		}
		if err := e.authorizeCached(ctx, ref, creds); err != nil {
			return image.LocalImage{}, err
		}
		return img, nil
	default:
		if e.puller == nil {
			return image.LoadCommitted(image.CacheDir(e.cfg.CacheDir, ref))
		}
		return e.puller.Pull(ctx, ref, creds, false)
	}
}

func (e *Engine) authorizeCached(ctx context.Context, ref string, creds image.RegistryCreds) error {
	if e.puller == nil {
		return fmt.Errorf("cached image %q cannot be authorized", ref)
	}
	if a, ok := e.puller.(imageAuthorizer); ok {
		return a.Authorize(ctx, ref, creds)
	}
	_, err := e.puller.Pull(ctx, ref, creds, false)
	return err
}
