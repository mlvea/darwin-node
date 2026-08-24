package digest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blob")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := FileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSidecar(p, sum); err != nil {
		t.Fatal(err)
	}
	if err := Verify(p, sum); err != nil {
		t.Fatal(err)
	}
	if err := Verify(p, "sha256:0000000000000000000000000000000000000000000000000000000000000000"); err == nil {
		t.Fatal("expected mismatch")
	}
}

// S004: overwriting the blob and regenerating the sidecar to the NEW hash
// must still fail when Verify is given the ORIGINAL expected digest.
func TestVerifyTamperSidecarFailsAgainstExpected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blob")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	original, err := FileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSidecar(p, original); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("xyz"), 0o644); err != nil {
		t.Fatal(err)
	}
	newSum, err := FileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	if newSum == original {
		t.Fatal("test payload must change the digest")
	}
	if err := WriteSidecar(p, newSum); err != nil {
		t.Fatal(err)
	}
	if err := Verify(p, original); err == nil {
		t.Fatal("expected mismatch after overwrite + regenerated sidecar")
	}
}

func TestVerifyFastPathSkipsHash(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blob")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := FileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSidecar(p, sum); err != nil {
		t.Fatal(err)
	}
	before := HashCount.Load()
	if err := Verify(p, sum); err != nil {
		t.Fatal(err)
	}
	if got := HashCount.Load(); got != before {
		t.Fatalf("fast path hashed: HashCount %d -> %d", before, got)
	}
	if err := Verify(p, sum); err != nil {
		t.Fatal(err)
	}
	if got := HashCount.Load(); got != before {
		t.Fatalf("second Verify hashed: HashCount %d -> %d", before, got)
	}
}

func TestWriteSidecarIncludesSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blob")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := FileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSidecar(p, sum); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(p + Suffix)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.HasPrefix(got, sum.String()) {
		t.Fatalf("sidecar %q", got)
	}
	if !strings.Contains(got, "size=3") {
		t.Fatalf("missing size in %q", got)
	}
	d, size, hasSize, err := readSidecar(p)
	if err != nil || !hasSize || size != 3 || d != sum {
		t.Fatalf("parse sidecar d=%s size=%d has=%t err=%v", d, size, hasSize, err)
	}
}

func TestReadSidecarLegacyDigestOnly(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blob")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := FileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p+Suffix, []byte(sum.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSidecar(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != sum {
		t.Fatalf("got %s want %s", got, sum)
	}
	// Legacy sidecar has no size, so Verify must hash (not skip).
	before := HashCount.Load()
	if err := Verify(p, sum); err != nil {
		t.Fatal(err)
	}
	if HashCount.Load() == before {
		t.Fatal("legacy sidecar without size must rehash")
	}
}

func TestVerifySizeMismatchRehashes(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blob")
	if err := os.WriteFile(p, []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, err := FileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteSidecar(p, sum); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("abcd"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := HashCount.Load()
	if err := Verify(p, sum); err == nil {
		t.Fatal("expected mismatch after size-changing overwrite")
	}
	if HashCount.Load() == before {
		t.Fatal("size mismatch must rehash")
	}
}
