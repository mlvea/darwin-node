package disk

import (
	"fmt"
	"math"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	// DefaultMaxBytes is the largest declared or decompressed byte count
	// CopySparse will write. A 128 GiB sparse macOS disk fits; a 1 PiB
	// annotation does not.
	DefaultMaxBytes int64 = 256 << 30

	// DefaultHeadroom is the free space that must remain on the filesystem
	// after a write. Pulls stop before the disk is filled to the last byte.
	DefaultHeadroom int64 = 10 << 30
)

// Available reports free bytes at path. Tests replace it.
var Available = statfsAvailable

// RequireSpace reports an error when path does not have incoming bytes
// plus headroom free. A non-positive headroom uses DefaultHeadroom.
func RequireSpace(path string, incoming, headroom int64) error {
	if headroom <= 0 {
		headroom = DefaultHeadroom
	}
	if incoming < 0 {
		return fmt.Errorf("negative size %d", incoming)
	}
	if incoming > math.MaxInt64-headroom {
		return fmt.Errorf("declared size %d exceeds limit", incoming)
	}
	free, err := Available(path)
	if err != nil {
		return fmt.Errorf("free disk: %w", err)
	}
	if free < incoming+headroom {
		return fmt.Errorf("need %d bytes plus %d headroom, %d free", incoming, headroom, free)
	}
	return nil
}

func statfsAvailable(path string) (int64, error) {
	probe := path
	if probe == "" {
		probe = "."
	}
	var last error
	for i := 0; i < 8; i++ {
		var st unix.Statfs_t
		err := unix.Statfs(probe, &st)
		if err == nil {
			return freeBlocks(st.Bavail, int64(st.Bsize))
		}
		last = err
		parent := filepath.Dir(probe)
		if parent == probe {
			break
		}
		probe = parent
	}
	if last == nil {
		last = fmt.Errorf("statfs %s", path)
	}
	return 0, last
}

func freeBlocks(bavail uint64, bsize int64) (int64, error) {
	if bsize <= 0 {
		return 0, fmt.Errorf("statfs block size %d", bsize)
	}
	bs := uint64(bsize)
	if bavail > uint64(math.MaxInt64)/bs {
		return math.MaxInt64, nil
	}
	return int64(bavail * bs), nil
}
