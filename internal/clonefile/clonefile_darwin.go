//go:build darwin

package clonefile

import "golang.org/x/sys/unix"

func clonefile(src, dst string) error {
	return unix.Clonefile(src, dst, 0)
}
