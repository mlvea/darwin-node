package leakcheck

import (
	"testing"
)

// A leaked module goroutine must show up as a diff against a clean
// snapshot. This is the negative case for the detector itself: the
// goroutine below stays alive for the whole process.
func TestLeaksDetectsPermanentGoroutine(t *testing.T) {
	before := interesting()
	stop := make(chan struct{})
	go func() {
		<-stop
	}()
	after := interesting()
	if got := leaks(before, after); len(got) == 0 {
		t.Fatal("detector missed a live module goroutine")
	}
	close(stop)
}

func TestLeaksIgnoresVanishedGoroutines(t *testing.T) {
	before := map[string]int{"sig": 1}
	after := map[string]int{"sig": 1, "other": 0}
	if got := leaks(before, after); len(got) != 0 {
		t.Fatalf("count decrease or zero-count must not leak: %v", got)
	}
}
