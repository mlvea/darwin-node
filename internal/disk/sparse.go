package disk

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Options bounds one sparse copy. Zero MaxBytes and Headroom use the defaults.
type Options struct {
	MaxBytes int64
	Headroom int64
}

const sparseBlock = 1 << 20 // 1 MiB

// zeroBlock is the comparison pad for all-zero chunks. It is never written to.
var zeroBlock = make([]byte, sparseBlock)

// CopySparse copies r to path, pre-truncating to sizeBytes when > 0 and
// skipping all-zero 1MiB chunks so APFS stores holes instead of zeroes.
// When sizeBytes <= 0, the file is truncated to the number of bytes read so
// trailing holes (skipped zero chunks) still count toward the length.
// Derived from Agoda macOS-vz-kubelet internal/disk (Apache-2.0).
//
// The copy rejects a declared size or a decompressed byte count above
// DefaultMaxBytes, stops when free space is below DefaultHeadroom, and
// does not observe a context. Callers that can be cancelled use
// CopySparseContext.
func CopySparse(path string, r io.Reader, sizeBytes int64) error {
	return CopySparseContext(context.Background(), path, r, sizeBytes, Options{})
}

// CopySparseContext is CopySparse with a context, a byte cap, and a free-space
// reserve. A cancelled context returns before the next read. sizeBytes, when
// positive, is the declared length and is rejected before the file is created
// when it is over the cap.
func CopySparseContext(ctx context.Context, path string, r io.Reader, sizeBytes int64, opt Options) error {
	maxBytes := opt.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	headroom := opt.Headroom
	if headroom <= 0 {
		headroom = DefaultHeadroom
	}
	if sizeBytes < 0 {
		return fmt.Errorf("negative size %d", sizeBytes)
	}
	if sizeBytes > maxBytes {
		return fmt.Errorf("declared size %d exceeds limit %d", sizeBytes, maxBytes)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := RequireSpace(dir, 0, headroom); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if sizeBytes > 0 {
		if err := f.Truncate(sizeBytes); err != nil {
			return err
		}
	}
	buf := make([]byte, sparseBlock)
	var off int64
	var sinceCheck int64
	for {
		n, err := readBlock(ctx, r, buf)
		if n > 0 {
			if off > maxBytes-int64(n) {
				return fmt.Errorf("decompressed size exceeds limit %d", maxBytes)
			}
			if !bytes.Equal(buf[:n], zeroBlock[:n]) {
				if _, werr := f.WriteAt(buf[:n], off); werr != nil {
					return werr
				}
				sinceCheck += int64(n)
				if sinceCheck >= 16<<20 {
					if err := RequireSpace(dir, 0, headroom); err != nil {
						return err
					}
					sinceCheck = 0
				}
			}
			off += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			if cerr := ctx.Err(); cerr != nil {
				return cerr
			}
			return err
		}
	}
	if sizeBytes <= 0 {
		if err := f.Truncate(off); err != nil {
			return err
		}
	}
	return f.Sync()
}

// readBlock fills buf, checking ctx before each Read so a cancel between
// short reads stops the copy. io.ReadFull would keep reading until the
// buffer was full.
func readBlock(ctx context.Context, r io.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		if err := ctx.Err(); err != nil {
			return n, err
		}
		nn, err := r.Read(buf[n:])
		n += nn
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
