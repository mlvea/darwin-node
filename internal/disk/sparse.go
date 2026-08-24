package disk

import (
	"bytes"
	"io"
	"os"
)

const sparseBlock = 1 << 20 // 1 MiB

// zeroBlock is the comparison pad for all-zero chunks. It is never written to.
var zeroBlock = make([]byte, sparseBlock)

// CopySparse copies r to path, pre-truncating to sizeBytes when > 0 and
// skipping all-zero 1MiB chunks so APFS stores holes instead of zeroes.
// When sizeBytes <= 0, the file is truncated to the number of bytes read so
// trailing holes (skipped zero chunks) still count toward the length.
// Derived from Agoda macOS-vz-kubelet internal/disk (Apache-2.0).
func CopySparse(path string, r io.Reader, sizeBytes int64) error {
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
	for {
		n, err := io.ReadFull(r, buf)
		if n > 0 {
			if !bytes.Equal(buf[:n], zeroBlock[:n]) {
				if _, werr := f.WriteAt(buf[:n], off); werr != nil {
					return werr
				}
			}
			off += int64(n)
		}
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
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
