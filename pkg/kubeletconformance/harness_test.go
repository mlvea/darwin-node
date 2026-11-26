package kubeletconformance

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/engine"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/node"
	"github.com/mlvea/darwin-node/pkg/provider"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/simvz"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

// wallBudget is how long a condition may take on the real clock. Backoff
// itself is not waited out here; those delays sit on the virtual clock.
const wallBudget = 4 * time.Second

// harness is one provider, engine, and SimVZ runtime. Cases do not share
// one: a virtual clock and the probe observer are process-wide or
// engine-wide, and the suite stays sequential.
type harness struct {
	t         *testing.T
	p         *provider.Provider
	e         *engine.Engine
	rt        *simWrap
	clk       *engine.VirtualClock
	cache     string
	mu        sync.Mutex
	phase     map[string]corev1.PodPhase
	seq       map[string][]corev1.PodPhase
	restarts  map[string]int32
	names     []string
	zeroGrace int64
}

type harnessOpt struct {
	// asVZ makes Name report the real vz runtime so the engine pulls an
	// image. SimVZ's own name skips that path.
	asVZ bool
	// faults is copied onto the simulator. FailStart is the boot-backoff case.
	faults simvz.Faults
	// writers is --cache-writers. Empty means no pod may commit a cache.
	writers []string
	// interrupted UIDs are planted under cache/pods before the engine
	// starts, which is what a process restart leaves behind.
	interrupted []string
	// pullTimeout bounds one registry attempt. Zero keeps the production default.
	pullTimeout time.Duration
	// agentTimeout is the guest-agent dial budget. Zero is a few seconds.
	agentTimeout time.Duration
	sc           sidecar.Runtime
}

// simWrap is the test double in front of SimVZ. Stop drops the engine's
// shutdown grace: restartVM passes five seconds of wall time, and that
// sleep is the guest's chance to exit, not the CrashLoop delay. The suite
// measures CrashLoop on the virtual clock, so the grace is not part of it.
type simWrap struct {
	*simvz.Runtime
	asVZ    bool
	mu      sync.Mutex
	machine runtime.Machine
	starts  atomic.Int32
}

func (s *simWrap) Name() types.RuntimeName {
	if s.asVZ {
		return types.RuntimeVZ
	}
	return s.Runtime.Name()
}

func (s *simWrap) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := s.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	w := &tracked{Machine: m, rt: s}
	s.mu.Lock()
	s.machine = w
	s.mu.Unlock()
	return w, nil
}

func (s *simWrap) last() runtime.Machine {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.machine
}

// tracked counts successful boots and refuses to sleep a stop grace.
// stopMu serializes Stop. Cleanup waits out the accept loop, and the
// engine watch can observe the session drop from inside that wait, see
// the machine already Stopped, and reach restartVM. restartVM calls
// Start only after its own Stop returns, so holding this lock until the
// in-flight Stop returns keeps that Start from registering a new accept
// loop that cleanup would then wait on forever.
type tracked struct {
	runtime.Machine
	rt     *simWrap
	stopMu sync.Mutex
}

func (m *tracked) Start(ctx context.Context) error {
	err := m.Machine.Start(ctx)
	if err == nil {
		m.rt.starts.Add(1)
	}
	return err
}

func (m *tracked) Stop(ctx context.Context, _ time.Duration) error {
	m.stopMu.Lock()
	defer m.stopMu.Unlock()
	return m.Machine.Stop(ctx, 0)
}

func newHarness(t *testing.T, opt harnessOpt) *harness {
	t.Helper()
	leakcheck.Check(t)
	cache := t.TempDir()
	for _, uid := range opt.interrupted {
		plantPodDir(t, cache, uid)
	}
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "simvz"
	cfg.CacheDir = cache
	cfg.AllowNATWorkloads = true
	cfg.MaxVMs = 2
	cfg.WarmSlots = 0
	cfg.ImageGC = false
	cfg.ProbeInterval = time.Hour
	cfg.AgentReadyTimeout = 3 * time.Second
	if opt.agentTimeout > 0 {
		cfg.AgentReadyTimeout = opt.agentTimeout
	}
	cfg.InitTimeout = 3 * time.Second
	cfg.CacheWriters = append([]string(nil), opt.writers...)
	if opt.pullTimeout > 0 {
		cfg.ImagePullTimeout = opt.pullTimeout
		cfg.ImageReadStall = opt.pullTimeout
	}
	sc := opt.sc
	if sc == nil {
		sc = sidecar.None{}
	}
	inner := simvz.New(simvz.Options{
		Dir:        t.TempDir(),
		Clock:      simvz.NewManualClock(),
		MaxRunning: 2,
		Faults:     opt.faults,
	})
	rt := &simWrap{Runtime: inner, asVZ: opt.asVZ}
	e := engine.New(cfg, slots, rt, sc, event.Nop{}, "10.0.0.8")
	clk := engine.NewVirtualClock(time.Unix(0, 0).UTC())
	e.SetVirtualClockForTest(clk)
	inv := node.Inventory{
		Host: node.Host{
			LogicalCPUs: 8,
			MemoryBytes: 32 << 30,
			DiskBytes:   200 << 30,
			Arch:        "arm64",
			InternalIP:  "10.0.0.8",
		},
		Cfg: cfg,
	}
	h := &harness{
		t:        t,
		p:        provider.New(cfg, e, inv, event.Nop{}),
		e:        e,
		rt:       rt,
		clk:      clk,
		cache:    cache,
		phase:    map[string]corev1.PodPhase{},
		seq:      map[string][]corev1.PodPhase{},
		restarts: map[string]int32{},
	}
	t.Cleanup(h.shutdown)
	return h
}

func (h *harness) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	h.mu.Lock()
	names := append([]string(nil), h.names...)
	h.mu.Unlock()
	for _, key := range names {
		ns, name, _ := splitKey(key)
		_ = h.p.DeletePod(ctx, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec:       corev1.PodSpec{TerminationGracePeriodSeconds: &h.zeroGrace},
		})
	}
	_ = h.e.Close()
}

func (h *harness) create(pod *corev1.Pod) {
	h.t.Helper()
	if pod.Spec.TerminationGracePeriodSeconds == nil {
		pod.Spec.TerminationGracePeriodSeconds = &h.zeroGrace
	}
	h.mu.Lock()
	h.names = append(h.names, pod.Namespace+"/"+pod.Name)
	h.mu.Unlock()
	if err := h.p.CreatePod(context.Background(), pod); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) pod(name, uid string, policy corev1.RestartPolicy) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       k8stypes.UID(uid),
		},
		Spec: corev1.PodSpec{
			RestartPolicy: policy,
			Containers: []corev1.Container{{
				Name:  "macos",
				Image: "registry.example.com/macos:test",
			}},
		},
	}
}

func (h *harness) get(ns, name string) (*corev1.Pod, error) {
	p, err := h.p.GetPod(context.Background(), ns, name)
	if err != nil {
		return nil, err
	}
	h.observe(p)
	return p, nil
}

func (h *harness) observe(p *corev1.Pod) {
	h.t.Helper()
	if p == nil {
		return
	}
	key := p.Namespace + "/" + p.Name
	phase := p.Status.Phase
	h.mu.Lock()
	defer h.mu.Unlock()
	if prev, ok := h.phase[key]; ok {
		if terminal(prev) && phase != prev {
			h.t.Fatalf("phase left %s for %s on %s", prev, phase, key)
		}
		if prev == corev1.PodRunning && phase == corev1.PodPending {
			h.t.Fatalf("phase regressed from Running to Pending on %s", key)
		}
	}
	seq := h.seq[key]
	if len(seq) == 0 || seq[len(seq)-1] != phase {
		h.seq[key] = append(seq, phase)
	}
	h.phase[key] = phase
	if len(p.Status.ContainerStatuses) == 0 {
		return
	}
	n := p.Status.ContainerStatuses[0].RestartCount
	if n < h.restarts[key] {
		h.t.Fatalf("restart count went from %d to %d on %s", h.restarts[key], n, key)
	}
	h.restarts[key] = n
}

func (h *harness) phases(ns, name string) []corev1.PodPhase {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]corev1.PodPhase(nil), h.seq[ns+"/"+name]...)
}

func (h *harness) wait(ns, name, why string, pred func(*corev1.Pod) bool) *corev1.Pod {
	h.t.Helper()
	deadline := time.Now().Add(wallBudget)
	var last *corev1.Pod
	var lastErr error
	for time.Now().Before(deadline) {
		p, err := h.get(ns, name)
		if err != nil {
			lastErr = err
		} else {
			last = p
			if pred(p) {
				return p
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("%s: %s (%v)", why, statusLine(last), lastErr)
	return nil
}

func (h *harness) waitPhase(ns, name string, phase corev1.PodPhase) *corev1.Pod {
	h.t.Helper()
	return h.wait(ns, name, "phase "+string(phase), func(p *corev1.Pod) bool {
		return p.Status.Phase == phase
	})
}

func (h *harness) waitExit(ns, name string) int32 {
	h.t.Helper()
	deadline := time.Now().Add(wallBudget)
	for time.Now().Before(deadline) {
		if code, ok := h.e.WorkloadExitCode(ns, name); ok {
			return code
		}
		time.Sleep(2 * time.Millisecond)
	}
	p, _ := h.get(ns, name)
	h.t.Fatalf("workload exit was not reported: %s", statusLine(p))
	return -1
}

// stopVM is the Linux stand-in for HaltGuest. The guest process exiting
// does not stop a SimVZ machine off Darwin, and the engine publishes
// Succeeded or Failed only once the machine is Stopped.
func (h *harness) stopVM() {
	h.t.Helper()
	m := h.rt.last()
	if m == nil {
		h.t.Fatal("no VM to stop")
	}
	st := m.Status().State
	if err := m.Stop(context.Background(), 0); err != nil && st == types.VMRunning {
		h.t.Fatalf("stop VM: %v", err)
	}
}

// nudgeWatch advances a parked probe tick. A watch that already left, or
// that is inside the session-close path, has no timer and is left alone.
func (h *harness) nudgeWatch() {
	h.t.Helper()
	deadline := time.Now().Add(200 * time.Millisecond)
	for {
		if h.clk.PendingTimersForTest() > 0 {
			h.clk.Advance(time.Hour)
			return
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) waitArmed() {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if h.clk.PendingTimersForTest() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	h.t.Fatal("virtual-clock timer was not armed")
}

// holdStarts fails if a successful boot happens during wall. It is the
// "the backoff has not fired" check, and wall is a few tens of milliseconds,
// not the backoff itself.
func (h *harness) holdStarts(want int32, wall time.Duration) {
	h.t.Helper()
	deadline := time.Now().Add(wall)
	for time.Now().Before(deadline) {
		if got := h.rt.starts.Load(); got != want {
			h.t.Fatalf("starts=%d, want %d while the backoff is still in force", got, want)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (h *harness) starts() int32 { return h.rt.starts.Load() }

// plantPodDir is the leftover cache/pods/<uid> a previous process would
// have left. Recover records the UID and then deletes the directory.
func plantPodDir(t *testing.T, cache, uid string) {
	t.Helper()
	dir := filepath.Join(cache, "pods", uid)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "marker"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func terminal(p corev1.PodPhase) bool {
	return p == corev1.PodSucceeded || p == corev1.PodFailed
}

func splitKey(key string) (string, string, bool) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:], true
		}
	}
	return "", key, false
}

func statusLine(p *corev1.Pod) string {
	if p == nil {
		return "pod missing"
	}
	return fmt.Sprintf("phase=%s reason=%s msg=%q restarts=%d ready=%v",
		p.Status.Phase, p.Status.Reason, p.Status.Message, restartCount(p), podReady(p))
}

func podReady(p *corev1.Pod) bool {
	if p == nil {
		return false
	}
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func condTime(p *corev1.Pod, typ corev1.PodConditionType) metav1.Time {
	for _, c := range p.Status.Conditions {
		if c.Type == typ {
			return c.LastTransitionTime
		}
	}
	return metav1.Time{}
}

func restartCount(p *corev1.Pod) int32 {
	if p == nil || len(p.Status.ContainerStatuses) == 0 {
		return 0
	}
	return p.Status.ContainerStatuses[0].RestartCount
}

func containerStarted(p *corev1.Pod) bool {
	if p == nil || len(p.Status.ContainerStatuses) == 0 || p.Status.ContainerStatuses[0].Started == nil {
		return false
	}
	return *p.Status.ContainerStatuses[0].Started
}

func findContainer(p *corev1.Pod, name string) *corev1.ContainerStatus {
	if p == nil {
		return nil
	}
	for i := range p.Status.ContainerStatuses {
		if p.Status.ContainerStatuses[i].Name == name {
			return &p.Status.ContainerStatuses[i]
		}
	}
	return nil
}

func terminatedState(p *corev1.Pod) *corev1.ContainerStateTerminated {
	if p == nil || len(p.Status.ContainerStatuses) == 0 {
		return nil
	}
	return p.Status.ContainerStatuses[0].State.Terminated
}
