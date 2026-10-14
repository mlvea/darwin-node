package main

import (
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestImageRejectedOffAppleSilicon(t *testing.T) {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		t.Skip("the image guard only fires off darwin/arm64")
	}
	// A directory is enough: the host check returns before the image is opened.
	err := runSoak(t.TempDir(), t.TempDir(), 1, 0, 2, 1, time.Second, 0, 8, true)
	if err == nil || !strings.Contains(err.Error(), "darwin/arm64") {
		t.Fatalf("got %v", err)
	}
}
