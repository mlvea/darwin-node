//go:build !linux && !darwin

package digest

import "os"

func fileID(os.FileInfo) (uint64, int64, int64, bool) { return 0, 0, 0, false }
