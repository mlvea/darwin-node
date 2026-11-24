package image

import (
	"context"
	"crypto/sha256"
	"io"
	"os"

	"github.com/darwin-node/darwin-node/internal/digest"

	ocidigest "github.com/opencontainers/go-digest"
)

// hashFile hashes path and returns when ctx is cancelled. The hash of a
// sparse hole returns immediately from the kernel, so a multi-gigabyte
// zero fill has to notice the context between reads or pod delete waits
// for the whole file.
func hashFile(ctx context.Context, path string) (ocidigest.Digest, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return hashReader(ctx, f)
}

func hashReader(ctx context.Context, r io.Reader) (ocidigest.Digest, error) {
	h := sha256.New()
	buf := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := h.Write(buf[:n]); werr != nil {
				return "", werr
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	digest.HashCount.Add(1)
	return ocidigest.NewDigest(ocidigest.SHA256, h), nil
}
