package image

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ocidigest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
)

// pushBlob stores content and returns its descriptor.
func pushBlob(t *testing.T, repo *remote.Repository, mediaType, title string, data []byte, ann map[string]string) ocispec.Descriptor {
	t.Helper()
	desc := ocispec.Descriptor{
		MediaType:   mediaType,
		Digest:      ocidigest.Digest("sha256:" + sha256Hex(data)),
		Size:        int64(len(data)),
		Annotations: map[string]string{},
	}
	if title != "" {
		desc.Annotations[ocispec.AnnotationTitle] = title
	}
	for k, v := range ann {
		desc.Annotations[k] = v
	}
	if err := repo.Push(context.Background(), desc, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	return desc
}

type layerSpec struct {
	mediaType string
	title     string
	data      []byte
	ann       map[string]string
}

// pushArtifact assembles an OCI manifest with the given layers and tags it.
func pushArtifact(t *testing.T, reg *casRegistry, ref string, configData []byte, layers []layerSpec) {
	t.Helper()
	repoName := strings.SplitN(ref, "/", 2)[1]
	repoName = strings.Split(repoName, ":")[0]
	repo, err := remote.NewRepository(reg.refFor(repoName, ""))
	if err != nil {
		t.Fatal(err)
	}
	repo.PlainHTTP = true

	cfgDesc := pushBlob(t, repo, MediaTypeConfig, "config.json", configData, nil)
	var descs []ocispec.Descriptor
	for _, l := range layers {
		descs = append(descs, pushBlob(t, repo, l.mediaType, l.title, l.data, l.ann))
	}
	manifest := ocispec.Manifest{
		MediaType: ocispec.MediaTypeImageManifest,
		Config:    cfgDesc,
		Layers:    descs,
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	tag := strings.SplitN(ref, ":", 3)
	manifestRef := tag[len(tag)-1]
	sum := "sha256:" + sha256Hex(raw)
	mDesc := ocispec.Descriptor{
		MediaType: ocispec.MediaTypeImageManifest,
		Digest:    ocidigest.Digest(sum),
		Size:      int64(len(raw)),
	}
	ctx := context.Background()
	if err := repo.Manifests().PushReference(ctx, mDesc, bytes.NewReader(raw), manifestRef); err != nil {
		t.Fatalf("push manifest %s: %v", manifestRef, err)
	}
}

func testConfigJSON(t *testing.T, diskFile string) []byte {
	t.Helper()
	cfg := Config{
		OS:                "darwin",
		HardwareModelData: "hw-" + diskFile,
		MachineIdData:     "mid",
		Storage: []BlobRef{
			{MediaType: MediaTypeDisk, File: "disk.img"},
			{MediaType: MediaTypeAux, File: "aux.img"},
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// publishBase pushes a full image artifact and returns its ref plus the
// disk bytes for later patching.
func publishBase(t *testing.T, reg *casRegistry, repo, tag string, disk []byte) (string, []byte) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "base")
	writeImageDirWithDisk(t, dir, disk)
	baseRef := reg.refFor(repo, tag)
	pushArtifact(t, reg, baseRef, readTestConfig(t, dir), []layerSpec{
		{mediaType: MediaTypeDisk, title: "disk.img", data: disk},
		{mediaType: MediaTypeAux, title: "aux.img", data: []byte("AUX")},
	})
	return baseRef, disk
}

func readTestConfig(t *testing.T, dir string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPullDeltaArtifactEndToEnd(t *testing.T) {
	reg := newCASRegistry()
	defer reg.close()

	baseDisk := patternDisk(6<<20, 5)
	baseRef, _ := publishBase(t, reg, "team/macos-base", "v1", baseDisk)

	// Target disk: one changed region; this is what a fresh bake produced.
	targetDisk := append([]byte(nil), baseDisk...)
	copy(targetDisk[2<<20:], randomBytes(4096))

	deltaDir := t.TempDir()
	ref := CreatePatchSilent(t, baseDisk, targetDisk, deltaDir)
	_ = ref
	patch, err := os.ReadFile(filepath.Join(deltaDir, "disk.img.patch"))
	if err != nil {
		t.Fatal(err)
	}

	deltaRef := reg.refFor("team/macos-app", "v2")
	pushArtifact(t, reg, deltaRef, testConfigJSON(t, "delta"), []layerSpec{
		{
			mediaType: MediaTypeDiskDelta,
			title:     "disk.delta.patch",
			data:      patch,
			ann: map[string]string{
				AnnotDeltaBaseRef:       baseRef,
				AnnotDeltaBaseDigest:    digestOf(baseDisk),
				AnnotUncompressedDigest: "sha256:" + digestOf(targetDisk),
				AnnotUncompressedSize:   fmt.Sprint(len(targetDisk)),
			},
		},
	})

	cacheRoot := t.TempDir()
	m := NewManager(cacheRoot)
	img, err := m.Pull(context.Background(), deltaRef, RegistryCreds{}, false)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(img.DiskPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, targetDisk) {
		t.Fatal("pulled delta disk does not match the baked target")
	}
	if _, err := LoadDir(img.Dir); err != nil {
		t.Fatalf("cache entry not a valid image dir: %v", err)
	}
	// The cloned base provenance must not survive the patch: a stale
	// (digest,size) pair passes the sidecar fast path whenever length is
	// unchanged, silently accepting corrupted content.
	if err := img.VerifyOptional(); err != nil {
		t.Fatalf("delta-pulled image fails its own verification: %v", err)
	}
	if img.DiskDig.String() != "sha256:"+digestOf(targetDisk) {
		t.Fatalf("provenance still carries the base digest: %s", img.DiskDig)
	}
	b, err := os.ReadFile(filepath.Join(img.Dir, "disk.img.digest"))
	if err != nil || !strings.HasPrefix(string(b), "sha256:"+digestOf(targetDisk)) {
		t.Fatalf("sidecar not refreshed: %q %v", b, err)
	}

	// Second pull must be served entirely from cache.
	reg.close()
	img2, err := m.Pull(context.Background(), deltaRef, RegistryCreds{}, false)
	if err != nil {
		t.Fatalf("cached re-pull failed: %v", err)
	}
	if img2.Dir != img.Dir {
		t.Fatal("second pull resolved outside the cache")
	}
}

// CreatePatchSilent wraps CreatePatch so test output stays readable.
func CreatePatchSilent(t *testing.T, base, target []byte, outDir string) PatchRef {
	t.Helper()
	baseDir := filepath.Join(t.TempDir(), "b")
	tgtDir := filepath.Join(t.TempDir(), "t")
	writeImageDirWithDisk(t, baseDir, base)
	writeImageDirWithDisk(t, tgtDir, target)
	devnull := os.DevNull
	old := os.Stderr
	f, _ := os.OpenFile(devnull, os.O_WRONLY, 0)
	os.Stderr = f
	ref, err := CreatePatch(filepath.Join(baseDir, "disk.img"), filepath.Join(tgtDir, "disk.img"), outDir, "disk.img")
	os.Stderr = old
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func TestPullDeltaRejectsWrongBase(t *testing.T) {
	reg := newCASRegistry()
	defer reg.close()

	baseDisk := patternDisk(4<<20, 11)
	baseRef, _ := publishBase(t, reg, "team/base", "v1", baseDisk)

	targetDisk := append([]byte(nil), baseDisk...)
	copy(targetDisk[512<<10:], randomBytes(2048))
	deltaDir := t.TempDir()
	CreatePatchSilent(t, baseDisk, targetDisk, deltaDir)
	patch, _ := os.ReadFile(filepath.Join(deltaDir, "disk.img.patch"))

	// Pin a base digest that does not match what the registry serves.
	wrongBase := append([]byte(nil), baseDisk...)
	wrongBase[0] ^= 0xff

	deltaRef := reg.refFor("team/app", "v2")
	pushArtifact(t, reg, deltaRef, testConfigJSON(t, "x"), []layerSpec{
		{
			mediaType: MediaTypeDiskDelta,
			title:     "disk.delta.patch",
			data:      patch,
			ann: map[string]string{
				AnnotDeltaBaseRef:       baseRef,
				AnnotDeltaBaseDigest:    digestOf(wrongBase),
				AnnotUncompressedDigest: "sha256:" + digestOf(targetDisk),
				AnnotUncompressedSize:   fmt.Sprint(len(targetDisk)),
			},
		},
	})

	cacheRoot := t.TempDir()
	m := NewManager(cacheRoot)
	_, err := m.Pull(context.Background(), deltaRef, RegistryCreds{}, false)
	if err == nil || !strings.Contains(err.Error(), "base mismatch") {
		t.Fatalf("want base mismatch error, got %v", err)
	}
	// A failed delta must not leave a partial cache entry behind.
	destDir := CacheDir(cacheRoot, deltaRef)
	if _, statErr := os.Stat(destDir); !os.IsNotExist(statErr) {
		t.Fatalf("failed pull left dest dir: %v", statErr)
	}
}

func TestPullDeltaMalformedAnnotationsFailLoudly(t *testing.T) {
	reg := newCASRegistry()
	defer reg.close()

	patch := patternDisk(64, 9)

	deltaRef := reg.refFor("team/app", "bad")
	pushArtifact(t, reg, deltaRef, testConfigJSON(t, "x"), []layerSpec{
		{
			mediaType: MediaTypeDiskDelta,
			title:     "patch.bin",
			data:      patch,
			ann:       map[string]string{}, // no annotations at all
		},
	})
	m := NewManager(t.TempDir())
	_, err := m.Pull(context.Background(), deltaRef, RegistryCreds{}, false)
	if err == nil || !strings.Contains(err.Error(), AnnotDeltaBaseRef) {
		t.Fatalf("missing annotation must fail loudly, got %v", err)
	}
}

func TestFindDeltaLayerAmbiguity(t *testing.T) {
	d := ocispec.Descriptor{MediaType: MediaTypeDiskDelta}
	_, found, err := findDeltaLayer([]ocispec.Descriptor{d, d})
	if !found || err == nil {
		t.Fatal("two delta layers must be rejected")
	}
	_, found, err = findDeltaLayer(nil)
	if found || err != nil {
		t.Fatal("no layers means no delta")
	}
}
