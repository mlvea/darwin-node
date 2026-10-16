package soak

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// DriftError is a failed fd, directory, slot, VM, cache, or goroutine check.
type DriftError struct {
	Kind   string
	Cycle  int
	Detail string
}

func (e *DriftError) Error() string {
	if e == nil {
		return "soak drift"
	}
	return fmt.Sprintf("soak drift %s at cycle %d: %s", e.Kind, e.Cycle, e.Detail)
}

func driftf(kind string, cycle int, format string, args ...any) error {
	return &DriftError{Kind: kind, Cycle: cycle, Detail: fmt.Sprintf(format, args...)}
}

// transient reports whether a drift sample may still be winding down
// (a just-closed fd, a slot release, a Serve goroutine). Directory and
// cache mismatches do not clear on their own.
func transient(err error) bool {
	d, ok := err.(*DriftError)
	if !ok || d == nil {
		return false
	}
	switch d.Kind {
	case driftFD, driftSlot, driftVM, driftGoroutine:
		return true
	default:
		return false
	}
}

const (
	driftFD        = "fd"
	driftDir       = "dir"
	driftSlot      = "slot"
	driftVM        = "vm"
	driftCache     = "cache"
	driftGoroutine = "goroutine"

	// defaultFDSlack absorbs allocator and resolver fds that show up once
	// and stay. Growth past this after the pre-cycle baseline is a leak.
	defaultFDSlack = 8
)

func checkSlots(used, want, max, cycle int) error {
	if used > max {
		return driftf(driftSlot, cycle, "used %d exceeds cap %d", used, max)
	}
	if used != want {
		return driftf(driftSlot, cycle, "used %d, want %d (cap %d)", used, want, max)
	}
	return nil
}

func checkRunning(running, want, max, cycle int) error {
	if running > max {
		return driftf(driftVM, cycle, "live VMs %d exceed cap %d", running, max)
	}
	if running != want {
		return driftf(driftVM, cycle, "live VMs %d, want %d", running, want)
	}
	return nil
}

func compareFDs(base, got, slack, cycle int) error {
	if slack < 0 {
		slack = 0
	}
	if got > base+slack {
		return driftf(driftFD, cycle, "open fds %d, baseline %d, slack %d", got, base, slack)
	}
	return nil
}

// openFDs counts file descriptors in this process. Linux uses /proc;
// Darwin uses /dev/fd. The count includes the directory fd opened to
// read it, so compare samples taken the same way.
func openFDs() (int, error) {
	var path string
	switch runtime.GOOS {
	case "linux":
		path = "/proc/self/fd"
	case "darwin":
		path = "/dev/fd"
	default:
		return 0, fmt.Errorf("fd census is not implemented on %s", runtime.GOOS)
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

type diskCensus struct {
	podDirs    int
	warmDirs   int
	leftovers  int // *.tmp and *.old under the cache dir
	storeFiles int
}

func takeCensus(cacheDir string) (diskCensus, error) {
	var c diskCensus
	var err error
	if c.podDirs, err = countChildren(filepath.Join(cacheDir, "pods")); err != nil {
		return c, err
	}
	if c.warmDirs, err = countChildren(filepath.Join(cacheDir, "warm")); err != nil {
		return c, err
	}
	if c.leftovers, err = countSuffixed(cacheDir, ".tmp", ".old"); err != nil {
		return c, err
	}
	if c.storeFiles, err = countFiles(filepath.Join(cacheDir, "cache-store")); err != nil {
		return c, err
	}
	return c, nil
}

func (c diskCensus) errIfDirty(cycle int) error {
	if c.podDirs != 0 {
		return driftf(driftDir, cycle, "%d pod %s left under cache/pods", c.podDirs, directories(c.podDirs))
	}
	if c.warmDirs != 0 {
		return driftf(driftDir, cycle, "%d warm %s left under cache/warm", c.warmDirs, directories(c.warmDirs))
	}
	if c.leftovers != 0 {
		return driftf(driftDir, cycle, "%d .tmp/.old snapshot leftover%s", c.leftovers, pluralN(c.leftovers))
	}
	return nil
}

func directories(n int) string {
	if n == 1 {
		return "directory"
	}
	return "directories"
}

func countChildren(dir string) (int, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	return len(ents), nil
}

func countSuffixed(root, a, b string) (int, error) {
	var n int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			return err
		}
		name := d.Name()
		if strings.HasSuffix(name, a) || strings.HasSuffix(name, b) {
			n++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return n, nil
}

func countFiles(root string) (int, error) {
	var n int
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			n++
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return n, nil
}

const stackMarker = "/darwin-node/"

// moduleStacks counts darwin-node goroutines other than the caller.
// The signature is the first two module frames, matching internal/leakcheck
// so a soak failure names the same sites the unit tests do.
func moduleStacks() map[string]int {
	self := currentGID()
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			buf = buf[:n]
			break
		}
		buf = make([]byte, 2*len(buf))
	}
	out := map[string]int{}
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, stackMarker) && !strings.Contains(g, "github.com/darwin-node/") {
			continue
		}
		if self != "" && strings.HasPrefix(g, "goroutine "+self+" ") {
			continue
		}
		sig := stackSignature(g)
		if sig == "" {
			continue
		}
		out[sig]++
	}
	return out
}

func currentGID() string {
	var buf [128]byte
	n := runtime.Stack(buf[:], false)
	line, _, _ := strings.Cut(string(buf[:n]), "\n")
	fields := strings.Fields(line)
	if len(fields) >= 2 && fields[0] == "goroutine" {
		return fields[1]
	}
	return ""
}

func stackSignature(goroutine string) string {
	var frames []string
	for _, line := range strings.Split(goroutine, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "github.com/darwin-node/") && !strings.Contains(line, stackMarker) {
			continue
		}
		frames = append(frames, line)
		if len(frames) == 2 {
			break
		}
	}
	if len(frames) == 0 {
		return ""
	}
	sort.Strings(frames)
	return strings.Join(frames, "\n")
}

func goroutineDrift(base, now map[string]int, cycle int) error {
	var extra []string
	for sig, n := range now {
		if n > base[sig] {
			extra = append(extra, fmt.Sprintf("(%d, baseline %d)\n%s", n, base[sig], sig))
		}
	}
	if len(extra) == 0 {
		return nil
	}
	sort.Strings(extra)
	n := len(extra)
	shown := extra
	if n > 4 {
		shown = append(append([]string{}, extra[:4]...), fmt.Sprintf("… %d more", n-4))
	}
	return driftf(driftGoroutine, cycle, "%d extra signature%s\n%s", n, pluralN(n), strings.Join(shown, "\n"))
}

func pluralN(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
