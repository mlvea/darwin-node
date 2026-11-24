package disk

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// A delta annotation can name a destination of 1 PiB. Truncating to that
// size and then hashing the hole reads zeros until the process is killed.
func TestCopySparseRejectsOnePiBDeclaredSize(t *testing.T) {
	const onePiB = int64(1) << 50
	err := CopySparse(filepath.Join(t.TempDir(), "big"), bytes.NewReader(nil), onePiB)
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("1 PiB declared size was not rejected by a size limit: %v", err)
	}
}

func TestCopySparseHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var reads int
	r := readerFunc(func(p []byte) (int, error) {
		reads++
		if reads == 2 {
			cancel()
		}
		if reads > 2 {
			return 0, fmt.Errorf("read %d times after cancel", reads)
		}
		n := 64
		if n > len(p) {
			n = len(p)
		}
		return n, nil
	})
	err := CopySparseContext(ctx, filepath.Join(t.TempDir(), "blob"), r, 0, Options{})
	if err != context.Canceled {
		t.Fatalf("got %v after %d reads, want context.Canceled", err, reads)
	}
}

func TestCopySparseStopsWhenFreeSpaceBelowHeadroom(t *testing.T) {
	orig := Available
	Available = func(string) (int64, error) { return 100, nil }
	t.Cleanup(func() { Available = orig })
	err := CopySparse(filepath.Join(t.TempDir(), "blob"), bytes.NewReader([]byte("not-all-zero")), 0)
	if err == nil || !strings.Contains(err.Error(), "headroom") {
		t.Fatalf("got %v, want a headroom error", err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
