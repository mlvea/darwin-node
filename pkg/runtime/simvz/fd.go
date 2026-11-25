package simvz

import (
	"fmt"
	"os"
	"runtime"
)

// FDCount is the number of open file descriptors in this process.
// Linux reads /proc/self/fd and Darwin reads /dev/fd. The count includes
// the directory descriptor opened to read it, so two calls are comparable.
func FDCount() (int, error) {
	var path string
	switch runtime.GOOS {
	case "linux":
		path = "/proc/self/fd"
	case "darwin":
		path = "/dev/fd"
	default:
		return 0, fmt.Errorf("fd count is not implemented on %s", runtime.GOOS)
	}
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return 0, err
	}
	return len(names), nil
}
