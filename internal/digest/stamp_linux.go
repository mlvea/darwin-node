//go:build linux

package digest

import (
	"os"
	"syscall"
)

func fileID(info os.FileInfo) (ino uint64, ctime, mtime int64, ok bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, 0, 0, false
	}
	return st.Ino, st.Ctim.Nano(), st.Mtim.Nano(), true
}
