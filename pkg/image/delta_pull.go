// Delta-aware pulls: an OCI artifact whose disk layer is a MediaTypeDiskDelta
// patch is resolved against its base image (pulled through the same cache),
// applied copy-on-write, and verified end to end before entering the cache.
package image

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/mlvea/darwin-node/internal/clonefile"

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
// A declared uncompressed size above DefaultMaxImageBytes is an error.
func findDeltaLayer(nodes []ocispec.Descriptor) (info *deltaLayerInfo, found bool, err error) {
	return findDeltaLayerLimited(nodes, 0)
}

func findDeltaLayerLimited(nodes []ocispec.Descriptor, maxDeclared int64) (info *deltaLayerInfo, found bool, err error) {
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
		if err := checkDeclaredSize(size, maxDeclared); err != nil {
			return nil, true, fmt.Errorf("delta layer %s: %w", AnnotUncompressedSize, err)
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
// ref is the image being pulled. A base that is ref, that is already on the
// pull stack, or whose manifest is itself a delta is rejected before another
// pull starts. A delta of a delta would otherwise recurse until the caller
// gave up, and the pod would keep its VM slot the whole time.
func (m *Manager) pullDeltaArtifact(ctx context.Context, ref, destDir string, info *deltaLayerInfo, creds RegistryCreds, stack map[string]struct{}) (LocalImage, error) {
	fail := func(err error) (LocalImage, error) {
		_ = os.RemoveAll(destDir)
		_ = os.RemoveAll(destDir + ".applied")
		_ = os.RemoveAll(destDir + ".deltastage")
		return LocalImage{}, err
	}

	if sameImageRef(info.baseRef, ref) {
		return fail(fmt.Errorf("delta base %q is the image itself", info.baseRef))
	}
	if stack != nil {
		if _, ok := stack[info.baseRef]; ok {
			return fail(fmt.Errorf("delta base %q cycles through %s", info.baseRef, ref))
		}
	}
	if err := m.limits().checkDeclared(info.destSize); err != nil {
		return fail(err)
	}
	isDelta, err := m.artifactIsDelta(ctx, info.baseRef, creds)
	if err != nil {
		return fail(fmt.Errorf("delta base %s: %w", info.baseRef, err))
	}
	if isDelta {
		return fail(fmt.Errorf("delta base %q is itself a delta", info.baseRef))
	}

	next := make(map[string]struct{}, len(stack)+1)
	for k := range stack {
		next[k] = struct{}{}
	}
	next[ref] = struct{}{}
	baseImg, err := m.pull(ctx, info.baseRef, creds, false, next)
	if err != nil {
		return fail(fmt.Errorf("delta base %s: %w", info.baseRef, err))
	}
	baseSum, err := hashFile(ctx, baseImg.DiskPath)
	if err != nil {
		return fail(err)
	}
	if baseSum.Encoded() != info.baseSHAHex {
		return fail(fmt.Errorf(
			"delta base mismatch: cached disk of %s hashes to %s but the patch pins %s",
			info.baseRef, baseSum.Encoded(), info.baseSHAHex))
	}

	if err := safePathSegment(info.title); err != nil {
		return fail(fmt.Errorf("delta patch blob name: %w", err))
	}
	patchPath := filepath.Join(destDir, info.title)
	if !fileExists(patchPath) {
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
	if err := applyDelta(ctx, baseImg.Dir, stage, appliedDir, m.limits()); err != nil {
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
	if err := completeProvenanceCtx(ctx, destDir, &prov); err != nil {
		return LocalImage{}, fmt.Errorf("provenance after apply: %w", err)
	}
	if err := WriteProvenance(destDir, prov); err != nil {
		return LocalImage{}, err
	}
	// The applied tree is a full disk. delta.json records which cached
	// image it was built from so GC can keep that base while this
	// directory exists. The patch bytes themselves are not kept. A
	// delta.json cloned in from the base would name the grandbase;
	// this write replaces it with the immediate base.
	if err := WriteDeltaManifest(destDir, DeltaManifest{BaseImage: info.baseRef}); err != nil {
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

func sameImageRef(a, b string) bool {
	return strings.TrimSpace(a) == strings.TrimSpace(b)
}

// artifactIsDelta reports whether ref's manifest carries a delta layer.
// It fetches the manifest only. It does not call Pull, so a base that
// names the image currently being pulled cannot deadlock on singleflight.
func (m *Manager) artifactIsDelta(ctx context.Context, ref string, creds RegistryCreds) (bool, error) {
	repo, err := m.repository(ref, creds)
	if err != nil {
		return false, err
	}
	// Fetch by tag. Resolve + Fetch addresses the manifest by digest, and a
	// descriptor whose media type is not recognized is fetched as a blob.
	_, rc, err := repo.FetchReference(ctx, repo.Reference.Reference)
	if err != nil {
		return false, err
	}
	defer rc.Close()
	body, err := io.ReadAll(io.LimitReader(rc, 4<<20+1))
	if err != nil {
		return false, err
	}
	if int64(len(body)) > 4<<20 {
		return false, fmt.Errorf("manifest exceeds limit %d", 4<<20)
	}
	var man ocispec.Manifest
	if err := json.Unmarshal(body, &man); err != nil {
		return false, fmt.Errorf("manifest: %w", err)
	}
	for _, layer := range man.Layers {
		if CanonicalMediaType(layer.MediaType) == MediaTypeDiskDelta {
			return true, nil
		}
	}
	return false, nil
}
