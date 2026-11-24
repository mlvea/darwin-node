//go:build darwin

package clonefile

import "golang.org/x/sys/unix"

func clonefile(src, dst string) error {
	// CLONE_NOFOLLOW: clonefile(2) otherwise copies the symlink target.
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
