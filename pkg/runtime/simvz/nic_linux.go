//go:build linux

package simvz

import "golang.org/x/sys/unix"

func datagramType() int { return unix.SOCK_DGRAM | unix.SOCK_CLOEXEC }
