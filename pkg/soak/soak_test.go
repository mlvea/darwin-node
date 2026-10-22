package soak

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/types"
)

func TestShortSoakNoDrift(t *testing.T) {
	dir := t.TempDir()
	inner := fake.New()
	rt := &countingRuntime{Runtime: inner}
	rep, err := Run(context.Background(), Config{
		CacheDir: dir,
		Runtime:  rt,
		Running:  inner.Running,
		Cycles:   4,
		MaxVMs:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cycles != 4 || rep.Width != 2 || rep.MaxVMs != 2 {
		t.Fatalf("report cycles=%d width=%d max=%d", rep.Cycles, rep.Width, rep.MaxVMs)
	}
	if rep.CapRejects != 4 {
		t.Fatalf("cap rejects %d, want one per cycle", rep.CapRejects)
	}
	if got := rt.creates.Load(); got != 8 {
		t.Fatalf("runtime Create calls %d, want 8 (4 cycles x 2 slots)", got)
	}
	if got := rt.maxLive.Load(); got > 2 {
		t.Fatalf("concurrent Create calls %d exceed the 2-VM cap", got)
	}
	if inner.Running() != 0 {
		t.Fatalf("fake runtime still has %d live VMs", inner.Running())
	}
	if rep.FDLast > rep.FDBase+defaultFDSlack {
		t.Fatalf("fds last %d base %d", rep.FDLast, rep.FDBase)
	}
	if rep.StoreFiles != 4 {
		t.Fatalf("store files %d, want 4 (marker + nested file, two caches)", rep.StoreFiles)
	}
	for slot, want := range map[int]string{0: "cycle-3\n", 1: "cycle-3\n"} {
		marker := filepath.Join(dir, "cache-store", "soak", cacheName(slot), markerFile)
		got, err := os.ReadFile(marker)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("slot %d marker %q", slot, got)
		}
		nested := filepath.Join(dir, "cache-store", "soak", cacheName(slot), nestedFile)
		got, err = os.ReadFile(nested)
		if err != nil || string(got) != nestedBody {
			t.Fatalf("slot %d nested %q %v", slot, got, err)
		}
	}
	ents, err := os.ReadDir(filepath.Join(dir, "pods"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Fatalf("pod dirs left: %d", len(ents))
	}
}

func TestSoakRespectsLoweredCap(t *testing.T) {
	rep, err := Run(context.Background(), Config{
		CacheDir: t.TempDir(),
		Cycles:   2,
		MaxVMs:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Width != 1 || rep.MaxVMs != 1 || rep.CapRejects != 2 {
		t.Fatalf("width %d max %d rejects %d", rep.Width, rep.MaxVMs, rep.CapRejects)
	}
}

func TestSoakDetectsLeftoverPodDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "pods", "orphan"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Run(context.Background(), Config{
		CacheDir: dir,
		Cycles:   1,
		MaxVMs:   1,
	})
	var drift *DriftError
	if !errors.As(err, &drift) || drift.Kind != driftDir {
		t.Fatalf("want dir drift, got %v", err)
	}
}

func TestSoakRejectsOverCapAndEmptyBudget(t *testing.T) {
	dir := t.TempDir()
	cases := []Config{
		{CacheDir: dir, Cycles: 1, MaxVMs: types.AppleMaxConcurrentVMs + 1},
		{CacheDir: dir, Cycles: 1, MaxVMs: 2, Width: 3},
		{CacheDir: dir, Cycles: 0, Duration: 0},
		{Cycles: 1},
	}
	for _, cfg := range cases {
		if _, err := Run(context.Background(), cfg); err == nil {
			t.Fatalf("config %+v: expected error", cfg)
		}
	}
}

func TestSoakDurationExpiresBeforeWork(t *testing.T) {
	start := time.Unix(1_700_000_000, 0)
	var calls int
	_, err := Run(context.Background(), Config{
		CacheDir: t.TempDir(),
		Duration: time.Minute,
		Cycles:   20,
		Now: func() time.Time {
			calls++
			if calls == 1 {
				return start
			}
			return start.Add(time.Hour)
		},
	})
	if err == nil || !strings.Contains(err.Error(), "ended before a cycle") {
		t.Fatalf("got %v", err)
	}
}

func TestSoakCanceledContextLeavesNoVMs(t *testing.T) {
	inner := fake.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dir := t.TempDir()
	_, err := Run(ctx, Config{
		CacheDir: dir,
		Runtime:  inner,
		Running:  inner.Running,
		Cycles:   5,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	if inner.Running() != 0 {
		t.Fatalf("running %d", inner.Running())
	}
}

func TestSoakRecoversAfterInjectedStartFailure(t *testing.T) {
	dir := t.TempDir()
	inner := fake.New()
	rt := &failStartRuntime{Runtime: inner}
	rt.left.Store(1)
	_, err := Run(context.Background(), Config{
		CacheDir: dir,
		Runtime:  rt,
		Running:  inner.Running,
		Cycles:   1,
		MaxVMs:   2,
	})
	if err == nil {
		t.Fatal("expected injected start failure")
	}
	if inner.Running() != 0 {
		t.Fatalf("running VMs after failure: %d", inner.Running())
	}
	ents, readErr := os.ReadDir(filepath.Join(dir, "pods"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	if len(ents) != 0 {
		t.Fatalf("pod dirs after injected failure: %d", len(ents))
	}

	rep, err := Run(context.Background(), Config{
		CacheDir: dir,
		Runtime:  inner,
		Running:  inner.Running,
		Cycles:   2,
		MaxVMs:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Cycles != 2 || inner.Running() != 0 {
		t.Fatalf("recovered cycles %d running %d", rep.Cycles, inner.Running())
	}
}

func TestFDDriftDetected(t *testing.T) {
	base, err := openFDs()
	if err != nil {
		t.Fatal(err)
	}
	var files []*os.File
	t.Cleanup(func() {
		for _, f := range files {
			_ = f.Close()
		}
	})
	for i := 0; i < defaultFDSlack+3; i++ {
		f, err := os.Open(os.DevNull)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	got, err := openFDs()
	if err != nil {
		t.Fatal(err)
	}
	if got <= base+defaultFDSlack {
		t.Fatalf("opened fds not visible: base %d got %d", base, got)
	}
	if err := compareFDs(base, got, defaultFDSlack, 1); err == nil {
		t.Fatal("expected fd drift")
	}
	if err := compareFDs(base, base, defaultFDSlack, 1); err != nil {
		t.Fatal(err)
	}
}

func TestCensusFlagsLeftovers(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{
		filepath.Join(dir, "pods", "orphan"),
		filepath.Join(dir, "warm", "1"),
		filepath.Join(dir, "cache-store", "ns"),
	} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "cache-store", "ns.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cache-store", "ns.old"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := takeCensus(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.podDirs != 1 || c.warmDirs != 1 || c.leftovers != 2 {
		t.Fatalf("census %+v", c)
	}
	if err := c.errIfDirty(3); err == nil {
		t.Fatal("expected dir drift")
	}
}

func TestSlotAndVMDriftDetected(t *testing.T) {
	if err := checkSlots(0, 0, 2, 1); err != nil {
		t.Fatal(err)
	}
	if err := checkSlots(2, 2, 2, 1); err != nil {
		t.Fatal(err)
	}
	if err := checkSlots(1, 0, 2, 4); err == nil {
		t.Fatal("expected slot drift")
	}
	if err := checkSlots(3, 3, 2, 4); err == nil {
		t.Fatal("expected cap drift")
	}
	if err := checkRunning(0, 0, 2, 1); err != nil {
		t.Fatal(err)
	}
	if err := checkRunning(2, 0, 2, 1); err == nil {
		t.Fatal("expected vm drift")
	}
}

func TestGoroutineDriftDetected(t *testing.T) {
	base := moduleStacks()
	started := make(chan struct{})
	done := make(chan struct{})
	exited := make(chan struct{})
	go func() {
		close(started)
		<-done
		close(exited)
	}()
	t.Cleanup(func() {
		close(done)
		<-exited
	})
	<-started
	err := goroutineDrift(base, moduleStacks(), 2)
	if err == nil {
		t.Fatal("expected goroutine drift")
	}
	var drift *DriftError
	if !errors.As(err, &drift) || drift.Kind != driftGoroutine {
		t.Fatalf("got %v", err)
	}
}

type countingRuntime struct {
	runtime.Runtime
	creates atomic.Int32
	cur     atomic.Int32
	maxLive atomic.Int32
}

func (r *countingRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	r.creates.Add(1)
	n := r.cur.Add(1)
	defer r.cur.Add(-1)
	for {
		old := r.maxLive.Load()
		if n <= old || r.maxLive.CompareAndSwap(old, n) {
			break
		}
	}
	return r.Runtime.Create(ctx, spec)
}

// failStartRuntime fails the first Start, then passes through. Same shape
// as pkg/engine/inject_test.go: the node must reclaim the slot and the
// pod directory, then accept a later cycle.
type failStartRuntime struct {
	runtime.Runtime
	left atomic.Int32
}

func (r *failStartRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &failStartMachine{Machine: m, left: &r.left}, nil
}

type failStartMachine struct {
	runtime.Machine
	left *atomic.Int32
}

func (m *failStartMachine) Start(ctx context.Context) error {
	if m.left.Add(-1) >= 0 {
		return errors.New("injected start failure")
	}
	return m.Machine.Start(ctx)
}
