// Delta-aware pulls: an OCI artifact whose disk layer is a MediaTypeDiskDelta
// patch is resolved against its base image (pulled through the same cache),
// applied copy-on-write, and verified end to end before entering the cache.
package image

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/darwin-node/darwin-node/internal/clonefile"

	"github.com/darwin-node/darwin-node/internal/digest"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

// deltaLayerInfo captures everything the pull path needs from a delta
// layer descriptor.
type deltaLayerInfo struct {
	title      string // blob file name ORAS wrote in destDir
	baseRef    string
	baseSHAHex string // hex sha256 of the base disk.img
	destSHAHex string // hex sha256 the patched disk must have
	destSize   int64
}

// findDeltaLayer inspects copied descriptors. found is false when the
// artifact carries no delta layer; a present-but-malformed delta yields an
// error so the pull fails loudly instead of falling through.
func findDeltaLayer(nodes []ocispec.Descriptor) (info *deltaLayerInfo, found bool, err error) {
	for _, d := range nodes {
		if CanonicalMediaType(d.MediaType) != MediaTypeDiskDelta {
			continue
		}
		if info != nil {
			return nil, true, fmt.Errorf("artifact carries more than one delta layer")
		}
		ann := d.Annotations
		baseRef := ann[AnnotDeltaBaseRef]
		baseDigest := strings.TrimPrefix(ann[AnnotDeltaBaseDigest], "sha256-")
		digestStr, size := UncompressedMeta(ann)
		destDigest := strings.TrimPrefix(digestStr, "sha256:")
		switch {
		case baseRef == "":
			return nil, true, fmt.Errorf("delta layer missing %s", AnnotDeltaBaseRef)
		case len(baseDigest) != 64:
			return nil, true, fmt.Errorf("delta layer %s must be 64 hex characters", AnnotDeltaBaseDigest)
		case len(destDigest) != 64:
			return nil, true, fmt.Errorf("delta layer %s must be the sha256 of the patched disk", AnnotUncompressedDigest)
		case size <= 0:
			return nil, true, fmt.Errorf("delta layer missing %s", AnnotUncompressedSize)
		}
		info = &deltaLayerInfo{
			title:      d.Annotations[ocispec.AnnotationTitle],
			baseRef:    baseRef,
			baseSHAHex: baseDigest,
			destSHAHex: destDigest,
			destSize:   size,
		}
		found = true
	}
	return info, found, nil
}

// pullDeltaArtifact turns a freshly copied delta artifact in destDir into a
// complete image dir by pulling baseRef and applying the patch.
func (m *Manager) pullDeltaArtifact(ctx context.Context, destDir string, info *deltaLayerInfo, creds RegistryCreds) (LocalImage, error) {
	fail := func(err error) (LocalImage, error) {
		_ = os.RemoveAll(destDir)
		_ = os.RemoveAll(destDir + ".applied")
		_ = os.RemoveAll(destDir + ".deltastage")
		return LocalImage{}, err
	}

	baseImg, err := m.Pull(ctx, info.baseRef, creds, false)
	if err != nil {
		return fail(fmt.Errorf("delta base %s: %w", info.baseRef, err))
	}
	baseSum, err := digest.FileSHA256(baseImg.DiskPath)
	if err != nil {
		return fail(err)
	}
	if baseSum.Encoded() != info.baseSHAHex {
		return fail(fmt.Errorf(
			"delta base mismatch: cached disk of %s hashes to %s but the patch pins %s",
			info.baseRef, baseSum.Encoded(), info.baseSHAHex))
	}

	patchPath := filepath.Join(destDir, info.title)
	if info.title == "" || !fileExists(patchPath) {
		return fail(fmt.Errorf("delta patch blob %q missing after copy", info.title))
	}
	if _, err := os.Stat(filepath.Join(destDir, "config.json")); err != nil {
		return fail(fmt.Errorf("delta artifact must carry the final image config.json"))
	}

	stage := destDir + ".deltastage"
	defer os.RemoveAll(stage)
	_ = os.RemoveAll(stage)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return fail(err)
	}
	if err := clonefile.File(patchPath, filepath.Join(stage, "disk.img.patch")); err != nil {
		return fail(fmt.Errorf("stage patch: %w", err))
	}
	baseStat, err := os.Stat(baseImg.DiskPath)
	if err != nil {
		return fail(err)
	}
	if err := WriteDeltaManifest(stage, DeltaManifest{
		BaseImage: info.baseRef,
		Patches: []PatchRef{{
			Name:     "disk.img",
			BaseSHA:  "sha256:" + info.baseSHAHex,
			BaseSize: baseStat.Size(),
			DestSHA:  "sha256:" + info.destSHAHex,
			DestSize: info.destSize,
		}},
	}); err != nil {
		return fail(err)
	}

	appliedDir := destDir + ".applied"
	_ = os.RemoveAll(appliedDir)
	if err := ApplyDelta(baseImg.Dir, stage, appliedDir); err != nil {
		return fail(fmt.Errorf("apply %s: %w", info.baseRef, err))
	}

	// The artifact's own config replaces whatever the base carried.
	if err := copyFile(filepath.Join(destDir, "config.json"), filepath.Join(appliedDir, "config.json")); err != nil {
		return fail(err)
	}
	if err := os.RemoveAll(destDir); err != nil {
		return fail(err)
	}
	if err := os.Rename(appliedDir, destDir); err != nil {
		return fail(err)
	}
	// The clone carried the base's provenance and sidecars; recompute both
	// so verification reflects the patched content.
	prov := Provenance{}
	if err := completeProvenance(destDir, &prov); err != nil {
		return LocalImage{}, fmt.Errorf("provenance after apply: %w", err)
	}
	if err := WriteProvenance(destDir, prov); err != nil {
		return LocalImage{}, err
	}
	img, err := LoadDir(destDir)
	if err != nil {
		return LocalImage{}, fmt.Errorf("load applied delta: %w", err)
	}
	return img, nil
}

func fileExists(path string) bool {
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular()
}
