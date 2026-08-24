package disk

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestCopySparse(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blob")
	payload := append(bytes.Repeat([]byte{0}, sparseBlock), []byte("hello")...)
	if err := CopySparse(p, bytes.NewReader(payload), int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("len got=%d want=%d", len(got), len(payload))
	}
}

// Issue 006: sizeBytes=0 must still keep trailing zeros (APFS images).
// Payload spans more than one sparseBlock so a trailing all-zero chunk is
// skipped and only Truncate(off) preserves the length.
func TestCopySparseTrailingZerosUnknownSize(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blob")
	payload := append([]byte{'A'}, bytes.Repeat([]byte{0}, sparseBlock+1024)...)
	if err := CopySparse(p, bytes.NewReader(payload), 0); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(payload) {
		t.Fatalf("len got=%d want=%d (trailing zeros dropped)", len(got), len(payload))
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("payload mismatch")
	}
}
