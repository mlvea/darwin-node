// Package soak is the adopt/delete harness docs/stability.md requires
// before a production-ready claim.
//
// Each cycle fills every configured VM slot (1 or 2, never more) with a
// pod that declares a cache volume, waits for the guest agent, writes a
// marker into that cache the way a guest write would land on the virtio-fs
// share, deletes the pod, and fails on fd, directory, or slot drift.
//
// Cache-volume pods cannot adopt a warm VM: virtio-fs shares are fixed at
// boot. The harness leaves the warm pool off so both slots stay available
// to the cycle. Warm adoption is covered by pkg/engine tests.
//
// The default runtime is fake, so the short path runs on Linux with no
// Virtualization.framework. Pass a vz runtime and a baked image directory
// for the 24h Apple Silicon run (cmd/darwin-soak, `make soak IMAGE=...`).
package soak

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

const (
	defaultNamespace = "soak"
	defaultImage     = "local/macos:soak"
	markerFile       = "soak-marker.txt"
	nestedFile       = "nested/keep.txt"
	nestedBody       = "keep\n"
	settleBudget     = 2 * time.Second
)

// Config selects how long to run and which runtime boots the pods.
type Config struct {
	// CacheDir is the node cache (pods, cache-store). Required.
	CacheDir string
	// Image is the pod image reference. A vz runtime wants a baked image
	// directory. Empty uses a fake-local ref that the fake runtime ignores.
	Image string
	// Runtime boots VMs. Nil selects the fake runtime.
	Runtime runtime.Runtime
	// Running, when set, is sampled against the slot table. Pass
	// fake.Runtime.Running. Leave nil for a runtime that cannot count VMs;
	// slot, fd, and directory checks still run.
	Running func() int
	// MaxVMs is 1 or 2. Zero defaults to 2. Above 2 is rejected.
	MaxVMs int
	// Width is how many pods run at once, 1..MaxVMs. Zero defaults to MaxVMs
	// so the cycle holds every slot the host is allowed to give out.
	Width int
	// Cycles to complete. Zero runs until Duration. Both may be set; the
	// run stops at whichever arrives first, after the in-flight cycle.
	Cycles int
	// Duration is the wall budget checked between cycles.
	Duration time.Duration
	// BootTimeout bounds one wait for PodRunning. Zero defaults to 30s.
	BootTimeout time.Duration
	// Grace is the delete grace period in seconds. Zero skips the guest
	// shutdown hook; the runtime Stop and the cache snapshot still run.
	Grace int64
	// FDSlack is how far open fds may sit above the pre-cycle baseline.
	// Zero defaults to 8.
	FDSlack int
	// AgentReadyTimeout is copied onto the engine. Zero defaults to 5s,
	// which is enough for the fake agent and too short for a real boot.
	AgentReadyTimeout time.Duration
	// Namespace defaults to "soak".
	Namespace string
	// Events receives engine lifecycle events. Nil is a no-op recorder.
	Events event.Recorder
	// Logf receives one line per cycle. Nil discards.
	Logf func(string, ...any)
	// Now is the clock. Nil is time.Now.
	Now func() time.Time
}

// Report is the summary printed at the end of a passing run.
type Report struct {
	Runtime    string
	Cycles     int
	Elapsed    time.Duration
	MaxVMs     int
	Width      int
	CapRejects int
	FDBase     int
	FDLast     int
	FDMax      int
	StoreFiles int
}

// Run executes the adopt/delete cycle until the cycle count or the
// duration budget is reached. On failure it deletes any pod it started
// and closes the engine before returning.
func Run(ctx context.Context, cfg Config) (Report, error) {
	cfg, err := normalize(cfg)
	if err != nil {
		return Report{}, err
	}
	rt := cfg.Runtime
	if rt == nil {
		rt = fake.New()
		if cfg.Running == nil {
			if fr, ok := rt.(*fake.Runtime); ok {
				cfg.Running = fr.Running
			}
		}
	}
	rec := cfg.Events
	if rec == nil {
		rec = event.Nop{}
	}
	slots, err := capacity.New(cfg.MaxVMs)
	if err != nil {
		return Report{}, err
	}
	ecfg := config.Default()
	ecfg.Runtime = rt.Name()
	ecfg.CacheDir = cfg.CacheDir
	ecfg.AllowNATWorkloads = true
	ecfg.AgentReadyTimeout = cfg.AgentReadyTimeout
	// Warm slots stay 0. A cache annotation makes the pod unable to adopt,
	// and a warm guest would consume one of the two slots this cycle needs.
	ecfg.WarmSlots = 0

	eng := engine.New(ecfg, slots, rt, sidecar.None{}, rec, "127.0.0.1")
	r := &runner{
		cfg:    cfg,
		eng:    eng,
		rt:     rt,
		ns:     cfg.Namespace,
		image:  cfg.Image,
		max:    cfg.MaxVMs,
		width:  cfg.Width,
		now:    cfg.Now,
		printf: cfg.Logf,
	}
	defer eng.Close()
	defer r.cleanup()

	fdBase, err := openFDs()
	if err != nil {
		return Report{}, err
	}
	r.fdBase = fdBase
	r.fdMax = fdBase
	r.stacks = moduleStacks()
	r.started = r.now()

	r.logf("SOAK start runtime=%s image=%s max_vms=%d width=%d cycles=%d duration=%s cache=%s",
		rt.Name(), cfg.Image, cfg.MaxVMs, cfg.Width, cfg.Cycles, cfg.Duration, cfg.CacheDir)

	var rep Report
	rep.Runtime = string(rt.Name())
	rep.MaxVMs = cfg.MaxVMs
	rep.Width = cfg.Width
	rep.FDBase = fdBase

	for {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		if r.budgetDone(rep.Cycles) {
			if rep.Cycles == 0 && cfg.Duration > 0 {
				return rep, fmt.Errorf("soak duration %s ended before a cycle completed", cfg.Duration)
			}
			break
		}
		if err := r.cycle(ctx, rep.Cycles); err != nil {
			return rep, err
		}
		rep.Cycles++
		rep.CapRejects = r.capRejects
		rep.FDLast = r.fdLast
		rep.FDMax = r.fdMax
		rep.StoreFiles = r.storeFiles
		rep.Elapsed = r.now().Sub(r.started)
		r.logf("SOAK cycle=%d slots=0/%d fds=%d/%d fd_max=%d pod_dirs=0 store_files=%d cap_rejects=%d elapsed=%s",
			rep.Cycles, r.max, r.fdLast, r.fdBase, r.fdMax, r.storeFiles, r.capRejects, rep.Elapsed.Round(time.Millisecond))
	}
	rep.Elapsed = r.now().Sub(r.started)
	r.logf("SOAK PASS cycles=%d elapsed=%s runtime=%s max_vms=%d fd_base=%d fd_max=%d cap_rejects=%d",
		rep.Cycles, rep.Elapsed.Round(time.Millisecond), rep.Runtime, rep.MaxVMs, rep.FDBase, rep.FDMax, rep.CapRejects)
	return rep, nil
}

func normalize(cfg Config) (Config, error) {
	if cfg.CacheDir == "" {
		return cfg, fmt.Errorf("soak cache dir is required")
	}
	if cfg.MaxVMs == 0 {
		cfg.MaxVMs = types.AppleMaxConcurrentVMs
	}
	if cfg.MaxVMs < 1 || cfg.MaxVMs > types.AppleMaxConcurrentVMs {
		return cfg, fmt.Errorf("soak max VMs must be 1 or 2 (Virtualization.framework cap), got %d", cfg.MaxVMs)
	}
	if cfg.Width == 0 {
		cfg.Width = cfg.MaxVMs
	}
	if cfg.Width < 1 || cfg.Width > cfg.MaxVMs {
		return cfg, fmt.Errorf("soak width %d is outside 1..%d", cfg.Width, cfg.MaxVMs)
	}
	if cfg.Cycles < 0 {
		return cfg, fmt.Errorf("soak cycles must be >= 0")
	}
	if cfg.Duration < 0 {
		return cfg, fmt.Errorf("soak duration must be >= 0")
	}
	if cfg.Cycles == 0 && cfg.Duration == 0 {
		return cfg, fmt.Errorf("soak requires a cycle count or a duration")
	}
	if cfg.BootTimeout <= 0 {
		cfg.BootTimeout = 30 * time.Second
	}
	if cfg.FDSlack <= 0 {
		cfg.FDSlack = defaultFDSlack
	}
	if cfg.AgentReadyTimeout <= 0 {
		cfg.AgentReadyTimeout = 5 * time.Second
	}
	if cfg.Namespace == "" {
		cfg.Namespace = defaultNamespace
	}
	if cfg.Image == "" {
		cfg.Image = defaultImage
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Grace < 0 {
		return cfg, fmt.Errorf("soak delete grace must be >= 0")
	}
	return cfg, nil
}

type runner struct {
	cfg    Config
	eng    *engine.Engine
	rt     runtime.Runtime
	ns     string
	image  string
	max    int
	width  int
	now    func() time.Time
	printf func(string, ...any)

	started    time.Time
	fdBase     int
	fdLast     int
	fdMax      int
	stacks     map[string]int
	storeFiles int
	storeReady bool
	capRejects int
	live       []string
}

func (r *runner) logf(format string, args ...any) {
	if r.printf != nil {
		r.printf(format, args...)
	}
}

func (r *runner) budgetDone(cycles int) bool {
	if r.cfg.Cycles > 0 && cycles >= r.cfg.Cycles {
		return true
	}
	if r.cfg.Duration > 0 && r.now().Sub(r.started) >= r.cfg.Duration {
		return true
	}
	return false
}

func (r *runner) cycle(ctx context.Context, n int) error {
	for slot := 0; slot < r.width; slot++ {
		name := podName(slot)
		pod := r.pod(name, uidFor(n, slot), slot)
		if err := r.eng.Create(ctx, pod, engine.Credentials{}); err != nil {
			return fmt.Errorf("cycle %d: create %s: %w", n, name, err)
		}
		r.live = append(r.live, name)
	}
	if r.width == r.max {
		const overflow = "soak-overflow"
		err := r.eng.Create(ctx, r.pod(overflow, fmt.Sprintf("soak-overflow-%d", n), 0), engine.Credentials{})
		if err == nil {
			r.live = append(r.live, overflow)
			return driftf(driftSlot, n, "create past %d VMs was admitted", r.max)
		}
		if !errors.Is(err, capacity.ErrVMCapacityExhausted) {
			return driftf(driftSlot, n, "create past %d VMs: %v", r.max, err)
		}
		r.capRejects++
	}
	for slot := 0; slot < r.width; slot++ {
		name := podName(slot)
		if err := r.waitRunning(ctx, name); err != nil {
			return fmt.Errorf("cycle %d: %w", n, err)
		}
	}
	if err := checkSlots(r.eng.Slots().Used(), r.width, r.max, n); err != nil {
		return err
	}
	if r.cfg.Running != nil {
		if err := checkRunning(r.cfg.Running(), r.width, r.max, n); err != nil {
			return err
		}
	}
	for slot := 0; slot < r.width; slot++ {
		name := podName(slot)
		uid := uidFor(n, slot)
		ectx, cancel := context.WithTimeout(ctx, r.cfg.BootTimeout)
		err := r.eng.ExecInVM(ectx, r.ns, name, []string{"/usr/bin/true"}, nil)
		cancel()
		if err != nil {
			return fmt.Errorf("cycle %d: exec %s: %w", n, name, err)
		}
		if err := r.checkRestored(uid, slot, n); err != nil {
			return err
		}
		if err := r.writeMarker(uid, slot, n); err != nil {
			return fmt.Errorf("cycle %d: cache write %s: %w", n, name, err)
		}
	}
	for slot := 0; slot < r.width; slot++ {
		name := podName(slot)
		if err := r.eng.Delete(ctx, r.ns, name, r.cfg.Grace); err != nil {
			return fmt.Errorf("cycle %d: delete %s: %w", n, name, err)
		}
		r.live = removeName(r.live, name)
	}
	if err := r.waitQuiescent(ctx, n); err != nil {
		return err
	}
	return r.checkStore(n)
}

func (r *runner) waitRunning(ctx context.Context, name string) error {
	deadline := time.Now().Add(r.cfg.BootTimeout)
	tick := time.NewTicker(15 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, err := r.eng.Get(r.ns, name)
		if err == nil && p.Status.Phase == corev1.PodRunning {
			return nil
		}
		if err == nil && p.Status.Phase == corev1.PodFailed {
			msg := p.Status.Message
			if msg == "" {
				msg = string(p.Status.Phase)
			}
			return fmt.Errorf("pod %s failed: %s", name, msg)
		}
		if time.Now().After(deadline) {
			phase := "missing"
			if p != nil {
				phase = fmt.Sprintf("%s (%s)", p.Status.Phase, p.Status.Message)
			}
			return fmt.Errorf("pod %s not running after %s: %s", name, r.cfg.BootTimeout, phase)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (r *runner) waitQuiescent(ctx context.Context, cycle int) error {
	deadline := time.Now().Add(settleBudget)
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		last = r.checkQuiescent(cycle)
		if last == nil || !transient(last) || time.Now().After(deadline) {
			return last
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (r *runner) checkQuiescent(cycle int) error {
	if err := checkSlots(r.eng.Slots().Used(), 0, r.max, cycle); err != nil {
		return err
	}
	if r.cfg.Running != nil {
		if err := checkRunning(r.cfg.Running(), 0, r.max, cycle); err != nil {
			return err
		}
	}
	disk, err := takeCensus(r.cfg.CacheDir)
	if err != nil {
		return fmt.Errorf("cycle %d: census: %w", cycle, err)
	}
	if err := disk.errIfDirty(cycle); err != nil {
		return err
	}
	fds, err := openFDs()
	if err != nil {
		return fmt.Errorf("cycle %d: fds: %w", cycle, err)
	}
	r.fdLast = fds
	if fds > r.fdMax {
		r.fdMax = fds
	}
	if err := compareFDs(r.fdBase, fds, r.cfg.FDSlack, cycle); err != nil {
		return err
	}
	if err := goroutineDrift(r.stacks, moduleStacks(), cycle); err != nil {
		return err
	}
	if r.storeReady && disk.storeFiles != r.storeFiles {
		return driftf(driftDir, cycle, "cache-store file count %d, want %d (snapshot is growing or shrinking)", disk.storeFiles, r.storeFiles)
	}
	return nil
}

func (r *runner) checkStore(cycle int) error {
	for slot := 0; slot < r.width; slot++ {
		marker := filepath.Join(r.storeDir(slot), markerFile)
		got, err := os.ReadFile(marker)
		if err != nil {
			return driftf(driftCache, cycle, "snapshot %s: %v", marker, err)
		}
		want := markerBody(cycle)
		if string(got) != want {
			return driftf(driftCache, cycle, "snapshot marker %q, want %q", got, want)
		}
		got, err = os.ReadFile(filepath.Join(r.storeDir(slot), nestedFile))
		if err != nil || string(got) != nestedBody {
			return driftf(driftCache, cycle, "snapshot nested file %q err=%v", got, err)
		}
	}
	disk, err := takeCensus(r.cfg.CacheDir)
	if err != nil {
		return err
	}
	if !r.storeReady {
		r.storeFiles = disk.storeFiles
		r.storeReady = true
	} else if disk.storeFiles != r.storeFiles {
		return driftf(driftDir, cycle, "cache-store file count %d, want %d", disk.storeFiles, r.storeFiles)
	}
	return nil
}

func (r *runner) checkRestored(uid string, slot, cycle int) error {
	if cycle == 0 {
		return nil
	}
	dir := r.podCache(uid, slot)
	got, err := os.ReadFile(filepath.Join(dir, markerFile))
	want := markerBody(cycle - 1)
	if err != nil || string(got) != want {
		return driftf(driftCache, cycle, "pod cache restore %q, want %q (%v)", got, want, err)
	}
	got, err = os.ReadFile(filepath.Join(dir, nestedFile))
	if err != nil || string(got) != nestedBody {
		return driftf(driftCache, cycle, "pod cache nested restore %q (%v)", got, err)
	}
	return nil
}

func (r *runner) writeMarker(uid string, slot, cycle int) error {
	dir := r.podCache(uid, slot)
	if err := os.MkdirAll(filepath.Join(dir, "nested"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, markerFile), []byte(markerBody(cycle)), 0o644); err != nil {
		return err
	}
	if cycle == 0 {
		return os.WriteFile(filepath.Join(dir, nestedFile), []byte(nestedBody), 0o644)
	}
	return nil
}

// podCache and storeDir follow the layout in pkg/engine (cache/pods/<uid>/caches/<name>
// and cache/cache-store/<namespace>/<name>). Each slot has its own cache name
// so two concurrent snapshots do not rename the same store directory.
func (r *runner) podCache(uid string, slot int) string {
	return filepath.Join(r.cfg.CacheDir, "pods", uid, "caches", cacheName(slot))
}

func (r *runner) storeDir(slot int) string {
	return filepath.Join(r.cfg.CacheDir, "cache-store", r.ns, cacheName(slot))
}

func (r *runner) pod(name, uid string, slot int) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: r.ns,
			UID:       k8stypes.UID(uid),
			Annotations: map[string]string{
				types.AnnotationCachePrefix + cacheName(slot): guestPath(slot),
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "macos",
				Image: r.image,
			}},
		},
	}
}

func (r *runner) cleanup() {
	if len(r.live) == 0 || r.eng == nil {
		return
	}
	timeout := r.cfg.BootTimeout
	if timeout < 2*time.Minute {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	for _, name := range r.live {
		_ = r.eng.Delete(ctx, r.ns, name, r.cfg.Grace)
	}
	r.live = nil
}

func podName(slot int) string { return fmt.Sprintf("soak-%d", slot) }

func uidFor(cycle, slot int) string { return fmt.Sprintf("soak-%d-%d", cycle, slot) }

func cacheName(slot int) string { return fmt.Sprintf("derived-%d", slot) }

func guestPath(slot int) string { return fmt.Sprintf("/Users/mac/DD/%d", slot) }

func markerBody(cycle int) string { return fmt.Sprintf("cycle-%d\n", cycle) }

func removeName(in []string, name string) []string {
	out := in[:0]
	for _, n := range in {
		if n != name {
			out = append(out, n)
		}
	}
	return out
}
