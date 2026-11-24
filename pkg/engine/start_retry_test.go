package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/image"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

type warnRec struct {
	mu    sync.Mutex
	warns []string
}

func (w *warnRec) Normal(context.Context, string, string) {}

func (w *warnRec) Warn(_ context.Context, reason, message string) {
	w.mu.Lock()
	w.warns = append(w.warns, reason+" "+message)
	w.mu.Unlock()
}

func (w *warnRec) has(reason string) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, m := range w.warns {
		if len(m) >= len(reason) && m[:len(reason)] == reason {
			return true
		}
	}
	return false
}

type vzNamed struct{ *fake.Runtime }

func (vzNamed) Name() types.RuntimeName { return types.RuntimeVZ }

type scriptPuller struct {
	mu    sync.Mutex
	img   image.LocalImage
	errs  []error
	calls int
}

func (p *scriptPuller) Pull(context.Context, string, image.RegistryCreds, bool) (image.LocalImage, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if len(p.errs) > 0 {
		err := p.errs[0]
		p.errs = p.errs[1:]
		if err != nil {
			return image.LocalImage{}, err
		}
	}
	return p.img, nil
}

func (p *scriptPuller) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func writeTinyImage(t *testing.T) image.LocalImage {
	t.Helper()
	dir := t.TempDir()
	cfg := `{
		"os":"darwin",
		"hardwareModelData":"abc",
		"machineIdData":"def",
		"storage":[
			{"mediatype":"application/vnd.darwin-node.disk.v1","file":"disk.img"},
			{"mediatype":"application/vnd.darwin-node.aux.v1","file":"aux.img"}
		]
	}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte("DISK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux.img"), []byte("AUX"), 0o644); err != nil {
		t.Fatal(err)
	}
	img, err := image.LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func clockedEngine(t *testing.T, rt runtime.Runtime, rec event.Recorder) (*Engine, *manualClock) {
	t.Helper()
	leakcheck.Check(t)
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 2 * time.Second
	cfg.AllowNATWorkloads = true
	if rec == nil {
		rec = event.Nop{}
	}
	e := New(cfg, slots, rt, sidecar.None{}, rec, "10.0.0.1")
	clk := newManualClock(time.Now())
	e.clock = clk
	t.Cleanup(func() { _ = e.Close() })
	return e, clk
}

func awaitBackoff(t *testing.T, e *Engine, clk *manualClock, name string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", name)
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("transient start error failed the pod: %s", p.Status.Message)
		}
		if clk.armed() {
			if e.slots.Used() != 1 {
				t.Fatalf("VM slot released while backing off: used=%d", e.slots.Used())
			}
			return p
		}
		time.Sleep(5 * time.Millisecond)
	}
	p, _ := e.Get("default", name)
	phase, msg := corev1.PodPhase(""), ""
	if p != nil {
		phase, msg = p.Status.Phase, p.Status.Message
	}
	t.Fatalf("start did not back off on the injected clock (phase %s %s, slots %d)", phase, msg, e.slots.Used())
	return nil
}

func TestTransientImagePullIsRetriedWithoutFailing(t *testing.T) {
	rec := &warnRec{}
	rt := vzNamed{Runtime: fake.New()}
	e, clk := clockedEngine(t, rt, rec)
	puller := &scriptPuller{
		img:  writeTinyImage(t),
		errs: []error{errors.New("dial tcp: i/o timeout"), errors.New("registry connection refused")},
	}
	e.puller = puller
	if err := e.Create(context.Background(), samplePod("pull", "uid-pull"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	wall := time.Now()
	p := awaitBackoff(t, e, clk, "pull")
	if p.Status.Reason != "BackOff" && !rec.has(event.ReasonBackOff) {
		t.Fatalf("retry was not visible: reason %q events %v", p.Status.Reason, rec.warns)
	}
	clk.Advance(time.Second)
	if !clk.WaitArmed(2 * time.Second) {
		p, _ = e.Get("default", "pull")
		t.Fatalf("second pull did not back off: phase %s calls %d", p.Status.Phase, puller.Calls())
	}
	clk.Advance(2 * time.Second)
	waitPhase(t, e, "default", "pull", corev1.PodRunning)
	if time.Since(wall) > time.Second {
		t.Fatalf("backoff slept on the wall clock: %s", time.Since(wall))
	}
	if got := puller.Calls(); got != 3 {
		t.Fatalf("pull calls %d, want 3", got)
	}
	if e.slots.Used() != 1 {
		t.Fatalf("running pod released its slot: %d", e.slots.Used())
	}
}

func TestTransientBootFailureIsRetried(t *testing.T) {
	inner := fake.New()
	rt := &injectRuntime{Runtime: inner, fail: failStart, times: 2}
	e, clk := clockedEngine(t, rt, nil)
	if err := e.Create(context.Background(), samplePod("boot", "uid-boot"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	awaitBackoff(t, e, clk, "boot")
	clk.Advance(time.Second)
	if !clk.WaitArmed(2 * time.Second) {
		t.Fatal("second boot failure did not back off")
	}
	clk.Advance(2 * time.Second)
	waitPhase(t, e, "default", "boot", corev1.PodRunning)
	if inner.Starts() != 1 {
		t.Fatalf("successful starts %d, want 1", inner.Starts())
	}
}

type flakyDialRuntime struct {
	*fake.Runtime
	left  atomic.Int32
	calls atomic.Int32
}

func (r *flakyDialRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &flakyDialMachine{Machine: m, left: &r.left, calls: &r.calls}, nil
}

type flakyDialMachine struct {
	runtime.Machine
	left  *atomic.Int32
	calls *atomic.Int32
}

func (m *flakyDialMachine) DialAgent(ctx context.Context) (*guest.Client, error) {
	m.calls.Add(1)
	if m.left.Add(-1) >= 0 {
		return nil, errors.New("connection refused")
	}
	return m.Machine.DialAgent(ctx)
}

func (m *flakyDialMachine) Status() types.VMStatus {
	st := m.Machine.Status()
	st.IP = ""
	return st
}

func TestTransientAgentDialIsRetried(t *testing.T) {
	inner := &flakyDialRuntime{Runtime: fake.New()}
	inner.left.Store(1)
	e, clk := clockedEngine(t, inner, nil)
	if err := e.Create(context.Background(), samplePod("dial", "uid-dial"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	wall := time.Now()
	awaitBackoff(t, e, clk, "dial")
	clk.Advance(time.Second)
	waitPhase(t, e, "default", "dial", corev1.PodRunning)
	if time.Since(wall) > time.Second {
		t.Fatalf("dial backoff slept on the wall clock: %s", time.Since(wall))
	}
	if inner.calls.Load() < 2 {
		t.Fatalf("dial calls %d, want at least 2", inner.calls.Load())
	}
}

type deadlineRuntime struct {
	*fake.Runtime
	remain atomic.Int64
}

func (r *deadlineRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &deadlineMachine{Machine: m, remain: &r.remain}, nil
}

type deadlineMachine struct {
	runtime.Machine
	remain *atomic.Int64
}

func (m *deadlineMachine) DialAgent(ctx context.Context) (*guest.Client, error) {
	if d, ok := ctx.Deadline(); ok {
		m.remain.Store(int64(time.Until(d)))
	} else {
		m.remain.Store(-1)
	}
	return m.Machine.DialAgent(ctx)
}

func TestAgentDialBudgetIsAgentReadyTimeout(t *testing.T) {
	rt := &deadlineRuntime{Runtime: fake.New()}
	e, _ := clockedEngine(t, rt, nil)
	e.cfg.AgentReadyTimeout = 90 * time.Second
	if err := e.Create(context.Background(), samplePod("slow", "uid-slow"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "slow", corev1.PodRunning)
	got := time.Duration(rt.remain.Load())
	if got < 60*time.Second {
		t.Fatalf("dial budget %s, want AgentReadyTimeout (90s) rather than the fixed 45s", got)
	}
	if got > 90*time.Second {
		t.Fatalf("dial budget %s exceeds AgentReadyTimeout", got)
	}
}

type permanentErr string

func (e permanentErr) Error() string   { return string(e) }
func (e permanentErr) Permanent() bool { return true }

func TestPermanentStartErrorsAreNotRetried(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{name: "auth", err: errors.New("401 Unauthorized")},
		{name: "invalid-image", err: errors.New("invalid image: parse image ref")},
		{name: "size-limit", err: errors.New("image exceeds the size limit")},
		{name: "declared-size", err: errors.New("declared size 99 exceeds limit 10")},
		{name: "compressed-limit", err: errors.New("compressed blob exceeds limit 10")},
		{name: "cycle-limit", err: errors.New("layer cycle limit exceeded")},
		{name: "delta-cycle", err: errors.New(`delta base "child" cycles through parent`)},
		{name: "delta-of-delta", err: errors.New(`delta base "child" is itself a delta`)},
		{name: "typed", err: permanentErr("boom")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := vzNamed{Runtime: fake.New()}
			e, clk := clockedEngine(t, rt, nil)
			puller := &scriptPuller{errs: []error{tc.err, errors.New("should not retry")}}
			e.puller = puller
			if err := e.Create(context.Background(), samplePod("perm", "uid-perm-"+tc.name), Credentials{}); err != nil {
				t.Fatal(err)
			}
			p := waitPhase(t, e, "default", "perm", corev1.PodFailed)
			if clk.armed() {
				t.Fatal("permanent error armed a backoff timer")
			}
			if got := puller.Calls(); got != 1 {
				t.Fatalf("pull calls %d, permanent error was retried", got)
			}
			if e.slots.Used() != 0 {
				t.Fatalf("slot held after permanent failure: %d", e.slots.Used())
			}
			if p.Status.Message == "" {
				t.Fatal("failed pod has no message")
			}
		})
	}
}

func TestPullTimeoutAndReadStallAreRetried(t *testing.T) {
	cases := []string{
		"pull ghcr.io/example/os:latest: context deadline exceeded",
		"registry read stalled for 2m0s",
	}
	for _, msg := range cases {
		t.Run(msg, func(t *testing.T) {
			rt := vzNamed{Runtime: fake.New()}
			e, clk := clockedEngine(t, rt, nil)
			puller := &scriptPuller{img: writeTinyImage(t), errs: []error{errors.New(msg)}}
			e.puller = puller
			if err := e.Create(context.Background(), samplePod("stall", "uid-stall"), Credentials{}); err != nil {
				t.Fatal(err)
			}
			awaitBackoff(t, e, clk, "stall")
			clk.Advance(time.Second)
			waitPhase(t, e, "default", "stall", corev1.PodRunning)
			if got := puller.Calls(); got != 2 {
				t.Fatalf("pull calls %d, want the stall or deadline retried once", got)
			}
		})
	}
}

func TestDeleteDuringStartBackoffReleasesSlot(t *testing.T) {
	rt := vzNamed{Runtime: fake.New()}
	e, clk := clockedEngine(t, rt, nil)
	e.puller = &scriptPuller{errs: []error{errors.New("i/o timeout"), errors.New("i/o timeout"), errors.New("i/o timeout")}}
	if err := e.Create(context.Background(), samplePod("hold", "uid-hold"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	awaitBackoff(t, e, clk, "hold")
	if err := e.Delete(context.Background(), "default", "hold", 0); err != nil {
		t.Fatal(err)
	}
	if e.slots.Used() != 0 {
		t.Fatalf("slot held after delete: %d", e.slots.Used())
	}
	if _, err := e.Get("default", "hold"); err == nil {
		t.Fatal("deleted pod still listed")
	}
}
