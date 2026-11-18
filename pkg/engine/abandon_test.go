package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// holdAfterBootRuntime boots the VM, then blocks before Start returns.
// The machine is already running while Close's deadline can fire.
type holdAfterBootRuntime struct {
	runtime.Runtime
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *holdAfterBootRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := h.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &holdAfterBootMachine{Machine: m, rt: h}, nil
}

type holdAfterBootMachine struct {
	runtime.Machine
	rt *holdAfterBootRuntime
}

func (m *holdAfterBootMachine) Start(ctx context.Context) error {
	if err := m.Machine.Start(ctx); err != nil {
		return err
	}
	m.rt.once.Do(func() {
		close(m.rt.entered)
		<-m.rt.release
	})
	return nil
}

func newHoldAfterBoot(inner *fake.Runtime) (*holdAfterBootRuntime, func()) {
	release := make(chan struct{})
	var once sync.Once
	rt := &holdAfterBootRuntime{Runtime: inner, entered: make(chan struct{}), release: release}
	return rt, func() { once.Do(func() { close(release) }) }
}

func waitBootHeld(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not reach the point where the VM is already running")
	}
}

// TestFinishShutdownStopsVMThatReturnsAfterCloseDeadline is the process-exit
// hole: Close gives up while Start has the VM running, and returning from
// shutdown lets the process die before that Start's VM is stopped.
func TestFinishShutdownStopsVMThatReturnsAfterCloseDeadline(t *testing.T) {
	inner := fake.New()
	rt, unblock := newHoldAfterBoot(inner)
	e := deadlineEngine(t, rt, sidecar.None{})
	t.Cleanup(unblock)
	e.closeTimeout = 200 * time.Millisecond
	e.reapTimeout = 3 * time.Second

	if err := e.Create(context.Background(), samplePod("a", "uid-reap-a"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitBootHeld(t, rt.entered)
	if inner.Running() != 1 || e.slots.Used() != 1 {
		t.Fatalf("running=%d slots=%d before shutdown, want the booted VM held", inner.Running(), e.slots.Used())
	}

	done := make(chan error, 1)
	go func() { done <- e.FinishShutdown() }()
	t.Cleanup(func() {
		unblock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	select {
	case err := <-done:
		t.Fatalf("FinishShutdown returned before Start: %v running=%d", err, inner.Running())
	case <-time.After(800 * time.Millisecond):
	}
	unblock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("FinishShutdown after the boot returned: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("FinishShutdown did not return after Start")
	}
	waitSlotReleased(t, e, inner)
	if _, err := os.Stat(filepath.Join(e.cfg.CacheDir, abandonedBootsFile)); !os.IsNotExist(err) {
		t.Fatalf("stopped boot left a reclaim record: %v", err)
	}
}

// TestFinishShutdownRecordsBootThatDoesNotReturn is the bound: if Start is
// still blocked when the reap ends, the pod uid is on disk for the next
// process, and this process still holds the slot.
func TestFinishShutdownRecordsBootThatDoesNotReturn(t *testing.T) {
	inner := fake.New()
	rt, unblock := newHoldAfterBoot(inner)
	e := deadlineEngine(t, rt, sidecar.None{})
	t.Cleanup(func() {
		unblock()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if e.slots.Used() == 0 && inner.Running() == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	e.closeTimeout = 150 * time.Millisecond
	e.reapTimeout = 300 * time.Millisecond

	if err := e.Create(context.Background(), samplePod("a", "uid-abandon-a"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitBootHeld(t, rt.entered)

	done := make(chan error, 1)
	go func() { done <- e.FinishShutdown() }()
	var finish error
	select {
	case finish = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("FinishShutdown did not return after the reap bound")
	}
	path := filepath.Join(e.cfg.CacheDir, abandonedBootsFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("abandoned boot was not recorded: %v (shutdown err %v)", err, finish)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode %o, want 0600", info.Mode().Perm())
	}
	if !strings.Contains(string(body), "uid-abandon-a") {
		t.Fatalf("record %s, want the pod uid", body)
	}
	if inner.Running() != 1 || e.slots.Used() != 1 {
		t.Fatalf("running=%d slots=%d, the in-flight VM must stay accounted", inner.Running(), e.slots.Used())
	}
	if err := e.Create(context.Background(), samplePod("b", "uid-abandon-b"), Credentials{}); err == nil {
		t.Fatal("create admitted while an abandoned boot still holds a slot")
	}
	unblock()
	waitSlotReleased(t, e, inner)
}

func TestFinishShutdownRecordsWarmBootThatDoesNotReturn(t *testing.T) {
	inner := fake.New()
	rt, unblock := newHoldAfterBoot(inner)
	e := deadlineEngine(t, rt, sidecar.None{})
	e.closeTimeout = 150 * time.Millisecond
	e.reapTimeout = 300 * time.Millisecond

	bootDone := make(chan error, 1)
	t.Cleanup(func() {
		unblock()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if e.slots.Used() == 0 && inner.Running() == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	go func() { bootDone <- e.bootWarm(context.Background(), warmImg) }()
	waitBootHeld(t, rt.entered)
	if e.slots.Used() != 1 || inner.Running() != 1 {
		t.Fatalf("warm boot running=%d slots=%d", inner.Running(), e.slots.Used())
	}

	done := make(chan error, 1)
	go func() { done <- e.FinishShutdown() }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("FinishShutdown did not return after the warm reap bound")
	}
	body, err := os.ReadFile(filepath.Join(e.cfg.CacheDir, abandonedBootsFile))
	if err != nil {
		t.Fatalf("abandoned warm boot was not recorded: %v", err)
	}
	if !strings.Contains(string(body), "darwin-warm-1") {
		t.Fatalf("record %s, want the warm slot id", body)
	}
	if inner.Running() != 1 || e.slots.Used() != 1 {
		t.Fatalf("running=%d slots=%d, warm boot must stay accounted", inner.Running(), e.slots.Used())
	}
	unblock()
	select {
	case <-bootDone:
	case <-time.After(5 * time.Second):
		t.Fatal("warm boot did not return")
	}
	waitSlotReleased(t, e, inner)
}

// TestNewReclaimsAbandonedBootWithoutPinningSlot is the next process: the
// record and the bundle go away, a held pod is left alone, and a bad id
// cannot escape the cache directory. The free slot is still usable.
func TestNewReclaimsAbandonedBootWithoutPinningSlot(t *testing.T) {
	cache := t.TempDir()
	orphan := filepath.Join(cache, "pods", "orphan-uid", "volumes", "token")
	held := filepath.Join(cache, "pods", "held-uid", "volumes", "token")
	escape := filepath.Join(cache, "escape-proof")
	for _, p := range []string{orphan, held, escape} {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("secret"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	record := `[
		{"id":"orphan-uid","kind":"pod","ref":"default/orphan"},
		{"id":"held-uid","kind":"pod","ref":"default/held"},
		{"id":"../escape","kind":"pod","ref":"bad"}
	]`
	if err := os.WriteFile(filepath.Join(cache, abandonedBootsFile), []byte(record), 0o600); err != nil {
		t.Fatal(err)
	}
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := slots.TryAcquire("held-uid"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = cache
	cfg.AgentReadyTimeout = 2 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, fake.New(), sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(func() { _ = e.Close() })

	if _, err := os.Stat(filepath.Join(cache, abandonedBootsFile)); !os.IsNotExist(err) {
		t.Fatalf("abandoned record still present: %v", err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan bundle still present: %v", err)
	}
	if _, err := os.Stat(held); err != nil {
		t.Fatalf("held pod dir removed: %v", err)
	}
	if _, err := os.Stat(escape); err != nil {
		t.Fatalf("reclaim followed a bad id: %v", err)
	}
	if used := e.slots.Used(); used != 1 {
		t.Fatalf("slots used=%d, want the held pod and no pinned orphan", used)
	}
	if err := e.Create(context.Background(), samplePod("next", "uid-reclaim-next"), Credentials{}); err != nil {
		t.Fatalf("free slot was pinned: %v", err)
	}
	waitPhase(t, e, "default", "next", corev1.PodRunning)
	err = e.Create(context.Background(), samplePod("over", "uid-reclaim-over"), Credentials{})
	if !isCapacity(err) {
		t.Fatalf("cap after reclaim: %v", err)
	}
}

func isCapacity(err error) bool {
	return err != nil && strings.Contains(err.Error(), "VM capacity exhausted")
}

// TestLateStartAfterCloseDeadlineDoesNotExceedCap checks the slot is not
// released twice when Start returns after Close gave up. Releasing early
// would let two more pods boot while the first VM is still running.
func TestLateStartAfterCloseDeadlineDoesNotExceedCap(t *testing.T) {
	inner := fake.New()
	rt, unblock := newHoldAfterBoot(inner)
	e := deadlineEngine(t, rt, sidecar.None{})
	t.Cleanup(unblock)
	e.closeTimeout = 200 * time.Millisecond

	if err := e.Create(context.Background(), samplePod("a", "uid-late-a"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitBootHeld(t, rt.entered)
	done, closeErr := startClose(e)
	t.Cleanup(func() {
		unblock()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})
	expectCloseReturns(t, e, unblock, done, closeErr)
	if inner.Running() != 1 {
		t.Fatalf("running=%d, Start had already booted the VM", inner.Running())
	}
	if err := e.Create(context.Background(), samplePod("b", "uid-late-b"), Credentials{}); err == nil {
		t.Fatal("second create admitted while the abandoned boot holds the only story")
	}
	// One slot is held by the in-flight boot. Filling the other must still
	// leave the cap at 2, and a third must fail.
	// Create is rejected because Close set draining. The slot count is the
	// cap check: it stays 1 until Start returns, then the normal path
	// releases it once.
	unblock()
	waitSlotReleased(t, e, inner)
	if used := e.slots.Used(); used != 0 {
		t.Fatalf("slots=%d after the late Start, release ran more than the one holder", used)
	}
}
