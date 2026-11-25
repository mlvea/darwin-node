package vzcontract

import (
	"os"
	"runtime"
	"testing"
	"time"
)

func fdCount() (int, error) {
	var path string
	switch runtime.GOOS {
	case "linux":
		path = "/proc/self/fd"
	case "darwin":
		path = "/dev/fd"
	default:
		return 0, nil
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

// checkFDs fails the test when the process still holds more descriptors
// than it did at the call, plus a small race-detector allowance. The
// directory fd used to count is closed before the number is returned, so
// two calls are comparable.
func checkFDs(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return
	}
	before, err := fdCount()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		var last int
		for {
			n, err := fdCount()
			if err != nil {
				t.Error(err)
				return
			}
			last = n
			if n <= before+fdSlack {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("open fds %d, baseline %d (slack %d)", last, before, fdSlack)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}
