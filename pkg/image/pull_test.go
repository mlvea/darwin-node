package image

import (
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/darwin-node/darwin-node/internal/digest"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

func TestMaybeDecompressGzip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "disk.img.gz")
	dest := filepath.Join(dir, "disk.img")
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte("hello-disk")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := maybeDecompress(src, dest); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello-disk" {
		t.Fatalf("got %q", b)
	}
}

func TestValidateRef(t *testing.T) {
	if err := ValidateRef("ghcr.io/org/macos:15"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateRef(""); err == nil {
		t.Fatal("expected error")
	}
}

func TestUncompressedMeta(t *testing.T) {
	d, n := UncompressedMeta(map[string]string{
		AgodaUncompDig:  "sha256:abc",
		AgodaUncompSize: "42",
	})
	if d != "sha256:abc" || n != 42 {
		t.Fatalf("%s %d", d, n)
	}
}

func TestIsPlainHTTPLoopbackOnly(t *testing.T) {
	if !isPlainHTTP("localhost:5000", nil) {
		t.Fatal("localhost")
	}
	if !isPlainHTTP("127.0.0.1:5000", nil) {
		t.Fatal("loopback")
	}
	if !isPlainHTTP("[::1]:5000", nil) {
		t.Fatal("ipv6 loopback")
	}
	if isPlainHTTP("10.1.2.3:5000", nil) {
		t.Fatal("10/8 must use TLS unless allow-listed")
	}
	if isPlainHTTP("192.168.1.4:5000", nil) {
		t.Fatal("192.168/16 must use TLS unless allow-listed")
	}
	if !isPlainHTTP("10.1.2.3:5000", []string{"10.1.2.3"}) {
		t.Fatal("explicit insecure list")
	}
}

func TestLayerRoleExactNotSubstring(t *testing.T) {
	if layerRole("disk-notes", "", "") != "" {
		t.Fatal("substring disk must not match")
	}
	if layerRole("notes.json", "", "") != "" {
		t.Fatal("any json must not become config")
	}
	if layerRole("disk.img", "", "") != "disk" {
		t.Fatal("exact disk.img")
	}
	if layerRole("blob", MediaTypeDisk, "") != "disk" {
		t.Fatal("media type")
	}
	if layerRole("blob", "", "config.json") != "config" {
		t.Fatal("title")
	}
}

func TestMaybeDecompressGzipNotInPlace(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "disk.img")
	var buf bytes.Buffer
	w := gzip.NewWriter(&buf)
	if _, err := w.Write([]byte("inplace-disk")); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := maybeDecompress(dest, dest); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "inplace-disk" {
		t.Fatalf("got %q", b)
	}
}

func TestMaterializePulledIgnoresSubstringNames(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disk-notes"), []byte("nope"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux-readme.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := materializePulled(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "disk.img")); err == nil {
		t.Fatal("disk-notes must not become disk.img")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("*.json must not become config.json")
	}
	if err := requireMaterializedDisk(dir); err == nil {
		t.Fatal("missing disk.img must be rejected")
	}
}

func TestCacheDirCreatedPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "images", "ref")
	if err := os.MkdirAll(dir, cacheDirPerm); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("perm %o want 0700", st.Mode().Perm())
	}
	if cacheDirPerm != 0o700 {
		t.Fatalf("cacheDirPerm %o", cacheDirPerm)
	}
}

func TestProvenanceFromDescriptorsUncompressed(t *testing.T) {
	const want = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	p := provenanceFromDescriptors([]ocispec.Descriptor{
		{
			MediaType: MediaTypeDisk,
			Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Annotations: map[string]string{
				ocispec.AnnotationTitle: "disk.img",
				AnnotUncompressedDigest: want,
				AnnotUncompressedSize:   "42",
			},
		},
		{
			MediaType:   MediaTypeConfig,
			Digest:      "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
			Size:        12,
			Annotations: map[string]string{ocispec.AnnotationTitle: "config.json"},
		},
		{
			MediaType: "application/vnd.oci.image.manifest.v1+json",
			Digest:    "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		},
	})
	if p.Files["disk.img"] != want {
		t.Fatalf("disk %q", p.Files["disk.img"])
	}
	if p.Sizes["disk.img"] != 42 {
		t.Fatalf("size %d", p.Sizes["disk.img"])
	}
	if p.Files["config.json"] == "" {
		t.Fatal("config digest")
	}
	if _, ok := p.Files["aux.img"]; ok {
		t.Fatal("no aux layer")
	}
}

func TestCompleteProvenanceWritesLockfile(t *testing.T) {
	dir := t.TempDir()
	writeTestImage(t, dir, "DISK", "AUX")
	p := Provenance{}
	if err := completeProvenance(dir, &p); err != nil {
		t.Fatal(err)
	}
	if p.Files["disk.img"] == "" || p.Files["aux.img"] == "" {
		t.Fatalf("%+v", p)
	}
	if err := WriteProvenance(dir, p); err != nil {
		t.Fatal(err)
	}
	got, err := LoadProvenance(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Files["disk.img"] != p.Files["disk.img"] {
		t.Fatalf("%+v", got)
	}
	img, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if img.DiskDig.String() != p.Files["disk.img"] {
		t.Fatalf("LoadDir digest %s", img.DiskDig)
	}
	before := digest.HashCount.Load()
	if err := img.VerifyOptional(); err != nil {
		t.Fatal(err)
	}
	if digest.HashCount.Load() != before {
		t.Fatal("second verify after completeProvenance should use sidecar fast path")
	}
}

func TestCompleteProvenanceRejectsSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	writeTestImage(t, dir, "DISK", "AUX")
	p := Provenance{Files: map[string]string{}, Sizes: map[string]int64{"disk.img": 99}}
	if err := completeProvenance(dir, &p); err == nil {
		t.Fatal("expected size mismatch")
	}
}

func TestCompleteProvenanceRejectsDigestMismatch(t *testing.T) {
	dir := t.TempDir()
	writeTestImage(t, dir, "DISK", "AUX")
	p := Provenance{Files: map[string]string{
		"disk.img": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}}
	if err := completeProvenance(dir, &p); err == nil {
		t.Fatal("expected digest mismatch")
	}
}

func TestMaterializePulledExactDisk(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte("DISK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux.img"), []byte("AUX"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := materializePulled(dir); err != nil {
		t.Fatal(err)
	}
	if err := requireMaterializedDisk(dir); err != nil {
		t.Fatal(err)
	}
}

func TestProvenanceFromDescriptorsSkipsCompressedDiskBlob(t *testing.T) {
	p := provenanceFromDescriptors([]ocispec.Descriptor{{
		MediaType: MediaTypeDisk,
		Digest:    "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Size:      99,
		Annotations: map[string]string{
			ocispec.AnnotationTitle: "disk.img",
		},
	}})
	if p.Files["disk.img"] != "" {
		t.Fatalf("compressed blob digest must not be the dest expected digest, got %q", p.Files["disk.img"])
	}
}
