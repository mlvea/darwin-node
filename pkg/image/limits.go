package image

import (
	"fmt"
	"time"

	"github.com/darwin-node/darwin-node/internal/disk"
)

const (
	// DefaultMaxImageBytes is the largest declared or decompressed disk
	// or aux file a pull will write.
	DefaultMaxImageBytes int64 = disk.DefaultMaxBytes

	// DefaultMaxCompressedBytes is the largest blob accepted from a registry.
	DefaultMaxCompressedBytes int64 = disk.DefaultMaxBytes

	// DefaultDiskHeadroom is the free space that must remain after a pull
	// writes. Zero on Manager means this value.
	DefaultDiskHeadroom int64 = disk.DefaultHeadroom

	// DefaultPullTimeout bounds one singleflight pull. The clock starts
	// with the pull and is not reset when another pod joins.
	DefaultPullTimeout = 30 * time.Minute

	// DefaultReadStall is how long a registry read may go without returning
	// a byte before the pull fails.
	DefaultReadStall = 2 * time.Minute
)

// byteLimits is the resolved pull budget. Zero fields mean the defaults.
type byteLimits struct {
	declared   int64
	compressed int64
	headroom   int64
}

func defaultLimits() byteLimits {
	return byteLimits{
		declared:   DefaultMaxImageBytes,
		compressed: DefaultMaxCompressedBytes,
		headroom:   DefaultDiskHeadroom,
	}
}

func (m *Manager) limits() byteLimits {
	if m == nil {
		return defaultLimits()
	}
	return byteLimits{
		declared:   positiveOr(m.MaxImageBytes, DefaultMaxImageBytes),
		compressed: positiveOr(m.MaxCompressedBytes, DefaultMaxCompressedBytes),
		headroom:   positiveOr(m.DiskHeadroom, DefaultDiskHeadroom),
	}
}

func (m *Manager) pullTimeout() time.Duration {
	if m != nil && m.PullTimeout > 0 {
		return m.PullTimeout
	}
	return DefaultPullTimeout
}

func (m *Manager) readStall() time.Duration {
	if m != nil && m.ReadStall > 0 {
		return m.ReadStall
	}
	return DefaultReadStall
}

func positiveOr(v, fallback int64) int64 {
	if v > 0 {
		return v
	}
	return fallback
}

func (l byteLimits) maxDeclared() int64 {
	return positiveOr(l.declared, DefaultMaxImageBytes)
}

func (l byteLimits) maxCompressed() int64 {
	return positiveOr(l.compressed, DefaultMaxCompressedBytes)
}

func (l byteLimits) diskHeadroom() int64 {
	return positiveOr(l.headroom, DefaultDiskHeadroom)
}

func (l byteLimits) checkDeclared(n int64) error {
	return checkDeclaredSize(n, l.maxDeclared())
}

func checkDeclaredSize(n, max int64) error {
	if max <= 0 {
		max = DefaultMaxImageBytes
	}
	if n < 0 {
		return fmt.Errorf("negative size %d", n)
	}
	if n > max {
		return fmt.Errorf("declared size %d exceeds limit %d", n, max)
	}
	return nil
}
