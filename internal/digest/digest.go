package digest

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync/atomic"

	ocidigest "github.com/opencontainers/go-digest"
)

const Suffix = ".digest"

// HashCount is incremented by FileSHA256. Tests use it to assert the
// sidecar fast path does not re-hash a matching file.
var HashCount atomic.Uint64

// FileSHA256 hashes the file at path.
func FileSHA256(path string) (ocidigest.Digest, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	HashCount.Add(1)
	return ocidigest.SHA256.FromReader(f)
}

// WriteSidecar writes path+".digest" as the digest plus the hashed file's size.
func WriteSidecar(path string, d ocidigest.Digest) error {
	body := d.String()
	if st, err := os.Stat(path); err == nil {
		body = fmt.Sprintf("%s\nsize=%d\n", d.String(), st.Size())
	}
	return os.WriteFile(path+Suffix, []byte(body), 0o644)
}

// ReadSidecar reads path+".digest". Legacy (digest-only) and size-bearing
// sidecars are both accepted.
func ReadSidecar(path string) (ocidigest.Digest, error) {
	d, _, _, err := readSidecar(path)
	return d, err
}

func readSidecar(path string) (ocidigest.Digest, int64, bool, error) {
	b, err := os.ReadFile(path + Suffix)
	if err != nil {
		return "", 0, false, err
	}
	return parseSidecar(b)
}

func parseSidecar(b []byte) (ocidigest.Digest, int64, bool, error) {
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", 0, false, fmt.Errorf("empty digest sidecar")
	}
	lines := strings.Split(s, "\n")
	d, err := ocidigest.Parse(strings.TrimSpace(lines[0]))
	if err != nil {
		return "", 0, false, fmt.Errorf("parse digest sidecar: %w", err)
	}
	var size int64
	hasSize := false
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		rest, ok := strings.CutPrefix(line, "size=")
		if !ok {
			continue
		}
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil {
			return "", 0, false, fmt.Errorf("parse digest sidecar size: %w", err)
		}
		size = n
		hasSize = true
	}
	return d, size, hasSize, nil
}

// Verify checks path against expected when set (the pull-time / provenance
// digest). The sidecar is only a cache keyed by (digest, size); mtime is
// never trusted.
//
// Fast path: sidecar digest == expected and sidecar size == current size.
// Slow path: hash the file and compare to expected (or to the sidecar when
// expected is empty).
func Verify(path string, expected ocidigest.Digest) error {
	if expected != "" {
		if got, size, hasSize, err := readSidecar(path); err == nil && got == expected && hasSize {
			st, statErr := os.Stat(path)
			if statErr == nil && st.Size() == size {
				return nil
			}
		}
		sum, err := FileSHA256(path)
		if err != nil {
			return err
		}
		if sum != expected {
			return fmt.Errorf("digest mismatch for %s: have %s want %s", path, sum, expected)
		}
		return WriteSidecar(path, sum)
	}

	sum, err := FileSHA256(path)
	if err != nil {
		return err
	}
	if got, err := ReadSidecar(path); err == nil && got != sum {
		return fmt.Errorf("digest mismatch for %s: have %s want %s", path, sum, got)
	}
	return WriteSidecar(path, sum)
}
