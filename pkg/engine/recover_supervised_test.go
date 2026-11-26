package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/observability"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/runtime/supervised"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

func shortRunDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dn")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

type runningSup struct {
	rt     *fake.Runtime
	srv    *supervisor.Server
	cancel context.CancelFunc
	done   chan struct{}
}

func startSup(t *testing.T, dir string, slot int, bridge bool) *runningSup {
	t.Helper()
	rt := fake.New()
	if bridge {
		rt.SetBridgeGuest(true)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv := &supervisor.Server{
		Slot:          slot,
		RunDir:        dir,
		RT:            rt,
		BinaryVersion: "sv-test",
		Deadman:       time.Hour,
	}
	s := &runningSup{rt: rt, srv: srv, cancel: cancel, done: make(chan struct{})}
	var serveErr error
	go func() {
		serveErr = srv.Serve(ctx)
		close(s.done)
	}()
	path := supervisor.SocketPath(dir, slot)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			t.Cleanup(func() {
				cancel()
				select {
				case <-s.done:
				case <-time.After(5 * time.Second):
					t.Errorf("supervisor slot %d did not exit: %v", slot, serveErr)
				}
			})
			return s
		}
		select {
		case <-s.done:
			t.Fatalf("supervisor slot %d exited: %v", slot, serveErr)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	t.Fatalf("socket %s did not appear", path)
	return nil
}

func supEngine(t *testing.T, cache, runDir string, meter bool) (*Engine, *supervised.Runtime) {
	t.Helper()
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = cache
	cfg.AllowNATWorkloads = true
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.ProbeInterval = 20 * time.Millisecond
	inner := supervised.New(supervised.Options{
		RunDir:        runDir,
		Slots:         2,
		Deadman:       time.Hour,
		VMRuntime:     types.RuntimeFake,
		BinaryVersion: "agent-test",
	})
	var rt runtime.Runtime = inner
	if meter {
		rt = observability.Meter(inner, observability.NewMetrics())
	}
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	return e, inner
}

func supPod(name, uid string) *corev1.Pod {
	p := samplePod(name, uid)
	p.Spec.RestartPolicy = corev1.RestartPolicyNever
	grace := int64(0)
	p.Spec.TerminationGracePeriodSeconds = &grace
	return p
}

func replaySupPod(name, uid string) *corev1.Pod {
	p := supPod(name, uid)
	p.Status = runningStatus(2)
	return p
}

func crashEngine(t *testing.T, e *Engine) {
	t.Helper()
	e.mu.RLock()
	recs := make([]*podRecord, 0, len(e.pods))
	for _, rec := range e.pods {
		recs = append(recs, rec)
	}
	e.mu.RUnlock()
	for _, rec := range recs {
		rec.mu.Lock()
		cancel := rec.cancel
		rec.cancel = nil
		agent := rec.agent
		m := rec.machine
		rec.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		done := make(chan struct{})
		go func(rec *podRecord) {
			rec.watchDone.Wait()
			rec.startDone.Wait()
			close(done)
		}(rec)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("pod watch did not return after cancel")
		}
		if agent != nil {
			_ = agent.Close()
		}
		supervised.Abandon(m)
	}
}

func writePodMarker(t *testing.T, cache, uid string) string {
	t.Helper()
	dir := filepath.Join(cache, "pods", uid, "overlay")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "disk.img")
	if err := os.WriteFile(path, []byte("SECRET-WORK"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertMarker(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("pod disk wiped: %v", err)
	}
	if string(b) != "SECRET-WORK" {
		t.Fatalf("pod disk %q", b)
	}
}

// TestRestartWithLiveSupervisorReattachesRunning is a node-agent restart
// while both slot supervisors are still up. The new process adopts the
// VMs, keeps them Running, and does not boot replacements or wipe disks.
func TestRestartWithLiveSupervisorReattachesRunning(t *testing.T) {
	leakcheck.Check(t)
	dir := shortRunDir(t)
	s0 := startSup(t, dir, 0, true)
	s1 := startSup(t, dir, 1, true)
	cache := t.TempDir()
	e1, _ := supEngine(t, cache, dir, false)
	if err := e1.Create(context.Background(), supPod("a", "uid-a"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	if err := e1.Create(context.Background(), supPod("b", "uid-b"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e1, "default", "a", corev1.PodRunning)
	waitPhase(t, e1, "default", "b", corev1.PodRunning)
	if s0.rt.Starts()+s1.rt.Starts() != 2 || s0.rt.Running()+s1.rt.Running() != 2 {
		t.Fatalf("starts %d+%d running %d+%d", s0.rt.Starts(), s1.rt.Starts(), s0.rt.Running(), s1.rt.Running())
	}
	markA := writePodMarker(t, cache, "uid-a")
	markB := writePodMarker(t, cache, "uid-b")
	crashEngine(t, e1)

	e2, rt := supEngine(t, cache, dir, false)
	t.Cleanup(func() { _ = e2.Close() })
	if err := e2.Create(context.Background(), replaySupPod("a", "uid-a"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	if err := e2.Create(context.Background(), replaySupPod("b", "uid-b"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	pa := waitSupRunning(t, e2, "a")
	pb := waitSupRunning(t, e2, "b")
	if pa.Status.Reason == event.ReasonNodeRestarted || pb.Status.Reason == event.ReasonNodeRestarted {
		t.Fatalf("live VMs were marked restarted: %s %s", pa.Status.Reason, pb.Status.Reason)
	}
	if s0.rt.Starts()+s1.rt.Starts() != 2 || s0.rt.Running()+s1.rt.Running() != 2 {
		t.Fatalf("reattach booted new VMs: starts %d+%d running %d+%d", s0.rt.Starts(), s1.rt.Starts(), s0.rt.Running(), s1.rt.Running())
	}
	assertMarker(t, markA)
	assertMarker(t, markB)
	if e2.slots.Used() != 2 || e2.slots.Free() != 0 {
		t.Fatalf("slots used %d free %d, want 2 held", e2.slots.Used(), e2.slots.Free())
	}
	if rt.HeldSlots() != 2 || rt.FreeSlots() != 0 {
		t.Fatalf("supervisor slots held %d free %d", rt.HeldSlots(), rt.FreeSlots())
	}
	err := e2.Create(context.Background(), supPod("c", "uid-c"), Credentials{})
	if !errors.Is(err, capacity.ErrVMCapacityExhausted) {
		t.Fatalf("third pod: %v", err)
	}
	for _, p := range []*corev1.Pod{pa, pb} {
		cs := containerStatus(p, "macos")
		if cs == nil || cs.RestartCount != 2 {
			t.Fatalf("restartCount %+v, reattach counted a new boot", cs)
		}
	}
	dctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := e2.Drain(dctx); err != nil {
		t.Fatal(err)
	}
	if e2.slots.Used() != 0 || e2.slots.Free() != 2 {
		t.Fatalf("after drain engine slots used %d free %d", e2.slots.Used(), e2.slots.Free())
	}
	if rt.HeldSlots() != 0 || rt.FreeSlots() != 2 {
		t.Fatalf("after drain supervisor slots held %d free %d", rt.HeldSlots(), rt.FreeSlots())
	}
	if s0.rt.Running()+s1.rt.Running() != 0 {
		t.Fatalf("drain left VMs running: %d+%d", s0.rt.Running(), s1.rt.Running())
	}
}

// TestMeterForwardsSupervisorRecover is the production wiring: run()
// wraps the supervised runtime in the metrics meter before New.
func TestMeterForwardsSupervisorRecover(t *testing.T) {
	leakcheck.Check(t)
	dir := shortRunDir(t)
	sup := startSup(t, dir, 0, true)
	cache := t.TempDir()
	// The first process is unwrapped so a crash can drop the supervised
	// machine. run() meters the runtime the new process is constructed with.
	e1, _ := supEngine(t, cache, dir, false)
	if err := e1.Create(context.Background(), supPod("m", "uid-m"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSupRunning(t, e1, "m")
	mark := writePodMarker(t, cache, "uid-m")
	crashEngine(t, e1)

	e2, rt := supEngine(t, cache, dir, true)
	t.Cleanup(func() { _ = e2.Close() })
	if err := e2.Create(context.Background(), replaySupPod("m", "uid-m"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSupRunning(t, e2, "m")
	if sup.rt.Starts() != 1 || sup.rt.Running() != 1 {
		t.Fatalf("starts %d running %d", sup.rt.Starts(), sup.rt.Running())
	}
	assertMarker(t, mark)
	if e2.slots.Used() != 1 || rt.HeldSlots() != 1 {
		t.Fatalf("engine used %d supervisor held %d", e2.slots.Used(), rt.HeldSlots())
	}
}

// TestRestartWithDeadSupervisorStaysFailed keeps the T-2 result when the
// slot supervisor is gone: Failed, slot free, disk removed, no new boot.
func TestRestartWithDeadSupervisorStaysFailed(t *testing.T) {
	leakcheck.Check(t)
	dir := shortRunDir(t)
	cache := t.TempDir()
	const uid = "uid-dead"
	plantPodDir(t, cache, uid)
	if err := supervisor.WriteLease(dir, 0, supervisor.Lease{
		PodUID: uid, PodName: "job", Namespace: "default",
		BootID: "boot-dead", HMACToken: "aa", ImageDigest: "local/macos:test",
		SupervisorPID: 1, SupervisorStartTimeNS: 1,
	}); err != nil {
		t.Fatal(err)
	}
	e, rt := supEngine(t, cache, dir, false)
	t.Cleanup(func() { _ = e.Close() })
	if _, err := os.Stat(filepath.Join(cache, "pods", uid)); !os.IsNotExist(err) {
		t.Fatalf("dead supervisor left the pod disk: %v", err)
	}
	if e.slots.Used() != 0 || rt.HeldSlots() != 0 {
		t.Fatalf("dead supervisor pinned a slot: engine %d supervisor %d", e.slots.Used(), rt.HeldSlots())
	}
	pod := supPod("job", uid)
	pod.Status = runningStatus(1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitFailedReason(t, e, "job", event.ReasonNodeRestarted)
	cs := containerStatus(p, "macos")
	if cs == nil || cs.RestartCount != 1 {
		t.Fatalf("restartCount %+v, want the previous count left as it was", cs)
	}
	if e.slots.Free() != 2 || rt.FreeSlots() != 2 {
		t.Fatalf("slots not free: engine %d supervisor %d", e.slots.Free(), rt.FreeSlots())
	}
}

// TestOrphanSupervisorIsStopped frees a live VM whose pod directory is gone.
func TestOrphanSupervisorIsStopped(t *testing.T) {
	leakcheck.Check(t)
	dir := shortRunDir(t)
	sup := startSup(t, dir, 0, false)
	ctx := context.Background()
	first := supervised.New(supervised.Options{
		RunDir:        dir,
		Slots:         2,
		Deadman:       time.Hour,
		VMRuntime:     types.RuntimeFake,
		BinaryVersion: "agent-old",
	})
	m, err := first.Create(ctx, types.VMSpec{
		ID:         types.MachineID{Namespace: "default", Name: "gone", UID: "uid-orphan"},
		ImageRef:   "local/macos:test",
		CPU:        2,
		AgentToken: "tok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if sup.rt.Running() != 1 {
		t.Fatalf("running %d", sup.rt.Running())
	}
	supervised.Abandon(m)

	cache := t.TempDir()
	e, rt := supEngine(t, cache, dir, false)
	t.Cleanup(func() { _ = e.Close() })
	if sup.rt.Running() != 0 {
		t.Fatalf("orphan VM still running: %d", sup.rt.Running())
	}
	if e.slots.Used() != 0 || e.slots.Free() != 2 {
		t.Fatalf("engine slots used %d free %d", e.slots.Used(), e.slots.Free())
	}
	if rt.HeldSlots() != 0 || rt.FreeSlots() != 2 {
		t.Fatalf("supervisor slots held %d free %d", rt.HeldSlots(), rt.FreeSlots())
	}
}

// TestSupervisorLostPodStatus is a supervisor that disappears while the
// pod is Running. The pod text says supervisor lost, and both slot tables
// give the slot up once.
func TestSupervisorLostPodStatus(t *testing.T) {
	leakcheck.Check(t)
	dir := shortRunDir(t)
	sup := startSup(t, dir, 0, true)
	cache := t.TempDir()
	e, rt := supEngine(t, cache, dir, false)
	t.Cleanup(func() { _ = e.Close() })
	if err := e.Create(context.Background(), supPod("die", "uid-lost"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitSupRunning(t, e, "die")
	sup.srv.SeverConnections()
	p := waitFailedReason(t, e, "die", "supervisor lost")
	if strings.Contains(p.Status.Message, "unexpectedly") || strings.Contains(p.Status.Reason, "VMFailed") {
		t.Fatalf("generic VM failure text: reason %q message %q", p.Status.Reason, p.Status.Message)
	}
	if e.slots.Free() != 2 || e.slots.Used() != 0 {
		t.Fatalf("engine slots used %d free %d", e.slots.Used(), e.slots.Free())
	}
	if rt.HeldSlots() != 0 {
		t.Fatalf("supervisor slot still held: %d", rt.HeldSlots())
	}
	// SeverConnections leaves the listener up. A killed supervisor has no
	// socket. Dial is that refusal: Create must not exec the test binary,
	// and a slot that is still held fails before Dial runs.
	rt.Dial = func(context.Context, int) (*supervisor.Client, error) {
		return nil, errors.New("supervisor socket closed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := rt.Create(ctx, types.VMSpec{
		ID:       types.MachineID{Namespace: "default", Name: "next", UID: "uid-next"},
		ImageRef: "local/macos:test",
	})
	if err == nil {
		t.Fatal("create reached a severed supervisor")
	}
	if strings.Contains(err.Error(), "no free supervisor slot") {
		t.Fatal("slot was not released for a new VM")
	}
	if rt.HeldSlots() != 0 || rt.FreeSlots() != 2 {
		t.Fatalf("after the failed create held %d free %d", rt.HeldSlots(), rt.FreeSlots())
	}
}

func waitSupRunning(t *testing.T, e *Engine, name string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	var last *corev1.Pod
	for time.Now().Before(deadline) {
		p, err := e.Get("default", name)
		if err == nil {
			last = p
			if p.Status.Phase == corev1.PodRunning {
				return p
			}
			if p.Status.Phase == corev1.PodFailed {
				t.Fatalf("pod %s failed: %s %s", name, p.Status.Reason, p.Status.Message)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last == nil {
		t.Fatalf("pod %s missing, want Running", name)
	}
	t.Fatalf("pod %s phase %s reason %q message %q, want Running", name, last.Status.Phase, last.Status.Reason, last.Status.Message)
	return last
}

func waitFailedReason(t *testing.T, e *Engine, name, want string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var last *corev1.Pod
	for time.Now().Before(deadline) {
		p, err := e.Get("default", name)
		if err != nil {
			t.Fatal(err)
		}
		last = p
		if p.Status.Phase == corev1.PodFailed && (p.Status.Reason == want || strings.Contains(p.Status.Message, want) || strings.Contains(p.Status.Reason, want)) {
			return p
		}
		time.Sleep(10 * time.Millisecond)
	}
	if last == nil {
		t.Fatalf("pod %s missing, want Failed %s", name, want)
	}
	t.Fatalf("pod %s phase %s reason %q message %q, want Failed %s", name, last.Status.Phase, last.Status.Reason, last.Status.Message, want)
	return last
}
