// Package leakcheck fails a test when goroutines from this module are
// still alive after the test finishes. It is deliberately narrow: only
// stacks that mention this module's packages count, so runtime and test
// harness goroutines never trip it.
package leakcheck

import (
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const marker = "/darwin-node/"

// Check registers a cleanup that compares live darwin-node goroutines
// against the snapshot taken at call time. Call it at the very start of
// tests whose lifecycle should leave nothing behind.
func Check(t *testing.T) {
	t.Helper()
	before := interesting()
	t.Cleanup(func() {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		var after map[string]int
		for {
			after = interesting()
			if len(after) <= len(before) || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		leaked := leaks(before, after)
		if len(leaked) > 0 {
			sort.Strings(leaked)
			t.Errorf("goroutine leak: %d darwin-node goroutine(s) still running", len(leaked))
			for _, s := range leaked {
				t.Errorf("leaked:\n%s", s)
			}
		}
	})
}

// leaks returns signatures present in after at higher counts than before.
func leaks(before, after map[string]int) []string {
	var out []string
	for stack, n := range after {
		if n > before[stack] {
			out = append(out, stack)
		}
	}
	return out
}

// interesting maps condensed stack signatures to counts for goroutines
// that belong to this module.
func interesting() map[string]int {
	out := map[string]int{}
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, marker) {
			continue
		}
		sig := signature(g)
		out[sig]++
	}
	return out
}

// signature keeps the first two darwin-node frames of a stack: enough to
// identify the leak site without churning on line numbers in helpers.
func signature(goroutine string) string {
	var frames []string
	for _, line := range strings.Split(goroutine, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "github.com/darwin-node/") &&
			!strings.Contains(line, marker) {
			continue
		}
		if i := strings.Index(line, " "); i > 0 && strings.HasSuffix(line[:i], ")") {
			// "created by" style lines carry no frame prefix; keep as-is.
			frames = append(frames, line)
			continue
		}
		frames = append(frames, line)
		if len(frames) == 2 {
			break
		}
	}
	sort.Strings(frames)
	return strings.Join(frames, "\n")
}
