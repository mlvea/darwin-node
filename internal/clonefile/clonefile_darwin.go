//go:build darwin

package clonefile

import "golang.org/x/sys/unix"

func clonefile(src, dst string) error {
	// CLONE_NOFOLLOW copies a symlink as a symlink instead of the target.
	// File refuses a symlink source; this flag covers the race in between.
	return unix.Clonefile(src, dst, unix.CLONE_NOFOLLOW)
}
