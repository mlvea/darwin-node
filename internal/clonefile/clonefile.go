// Package clonefile wraps APFS clonefile with a copy fallback.
package clonefile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// File copies src to dst using clonefile on Darwin/APFS, else a regular copy.
func File(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	_ = os.Remove(dst)
	if runtime.GOOS == "darwin" {
		if err := clonefile(src, dst); err == nil {
			return nil
		}
		// Fall through to copy on non-APFS (e.g. some CI volumes).
	}
	return copyFile(src, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open src: %w", err)
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
	if err != nil {
		return fmt.Errorf("open dst: %w", err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}
