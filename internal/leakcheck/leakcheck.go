// Package leakcheck fails a test when goroutines from this module are
// still alive after the test finishes. It is deliberately narrow: only
// stacks that mention this module's packages count, so runtime and test
// harness goroutines never trip it.
package leakcheck

import (
	"runtime"
	"sort"
	"strconv"
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
	// Identity, not stack text. A guest heartbeat parks in select and
	// briefly enters Health on each tick. Those are different signatures
	// for the same goroutine, so a diff of stack text reports a leak in
	// whichever later test happens to sample the tick.
	before := liveIDs()
	t.Cleanup(func() {
		t.Helper()
		deadline := time.Now().Add(2 * time.Second)
		var leaked []string
		for {
			leaked = newSince(before)
			if len(leaked) == 0 || time.Now().After(deadline) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if len(leaked) > 0 {
			sort.Strings(leaked)
			t.Errorf("goroutine leak: %d darwin-node goroutine(s) still running", len(leaked))
			for _, s := range leaked {
				t.Errorf("leaked:\n%s", s)
			}
		}
	})
}

// newSince returns signatures of module goroutines whose ids were not in
// before. A goroutine that only changed stacks is not a leak.
func newSince(before map[uint64]struct{}) []string {
	var out []string
	for id, sig := range liveModule() {
		if _, ok := before[id]; ok {
			continue
		}
		out = append(out, sig)
	}
	return out
}

func liveIDs() map[uint64]struct{} {
	live := liveModule()
	out := make(map[uint64]struct{}, len(live))
	for id := range live {
		out[id] = struct{}{}
	}
	return out
}

// liveModule maps goroutine ids to stack signatures for this module.
func liveModule() map[uint64]string {
	out := map[uint64]string{}
	for _, g := range moduleStacks(allStacks()) {
		id, ok := goroutineID(g)
		if !ok {
			continue
		}
		out[id] = signature(g)
	}
	return out
}

func goroutineID(g string) (uint64, bool) {
	const prefix = "goroutine "
	if !strings.HasPrefix(g, prefix) {
		return 0, false
	}
	rest := g[len(prefix):]
	i := strings.IndexByte(rest, ' ')
	if i <= 0 {
		return 0, false
	}
	id, err := strconv.ParseUint(rest[:i], 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
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
	for _, g := range moduleStacks(allStacks()) {
		out[signature(g)]++
	}
	return out
}

func allStacks() string {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, 2*len(buf))
	}
}

// moduleStacks drops runtime and leakcheck frames. The snapshot goroutine's
// own stack mentions Check or interesting. On a checkout whose path does
// not contain "/darwin-node/", those two frames used to look like a new
// leak. GitHub's runner path contains the repo name, so the source line
// crowded that signature out and CI stayed green.
func moduleStacks(all string) []string {
	var out []string
	for _, g := range strings.Split(all, "\n\n") {
		if !strings.Contains(g, marker) {
			continue
		}
		if strings.Contains(g, "internal/leakcheck.Check") ||
			strings.Contains(g, "internal/leakcheck.interesting(") ||
			strings.Contains(g, "internal/leakcheck.liveModule(") ||
			strings.Contains(g, "internal/leakcheck.liveIDs(") ||
			strings.Contains(g, "internal/leakcheck.newSince(") {
			continue
		}
		out = append(out, g)
	}
	return out
}

// signature keeps the first two darwin-node frames of a stack: enough to
// identify the leak site without churning on line numbers in helpers.
func signature(goroutine string) string {
	var frames []string
	for _, line := range strings.Split(goroutine, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "github.com/mlvea/darwin-node/") &&
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
