package engine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// oneSlotEngine is a 1-VM node with an injected clock. A pod that holds
// the only slot while it pulls or backs off leaves the node unable to
// start anything else.
func oneSlotEngine(t *testing.T, rt runtime.Runtime) (*Engine, *manualClock) {
	t.Helper()
	leakcheck.Check(t)
	slots, err := capacity.New(1)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 2 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	clk := newManualClock(time.Now())
	e.clock = clk
	t.Cleanup(func() { _ = e.Close() })
	return e, clk
}

func awaitArmed(t *testing.T, clk *manualClock) {
	t.Helper()
	if !clk.WaitArmed(2 * time.Second) {
		t.Fatal("backoff did not arm a timer on the injected clock")
	}
}

// A pod stuck retrying an image pull must not hold the only VM slot.
// Another pod has to be able to boot while that pull backs off.
func TestPullBackoffDoesNotBlockAnotherPod(t *testing.T) {
	rt := vzNamed{Runtime: fake.New()}
	e, clk := oneSlotEngine(t, rt)
	// The stuck pod's image keeps failing. The other image pulls, so a
	// free slot is enough for that pod to boot.
	e.puller = &scriptPuller{
		img:     writeTinyImage(t),
		failRef: "local/macos:stuck",
	}
	stuck := samplePod("pull", "uid-pull")
	stuck.Spec.Containers[0].Image = "local/macos:stuck"
	if err := e.Create(context.Background(), stuck, Credentials{}); err != nil {
		t.Fatal(err)
	}
	awaitArmed(t, clk)
	if used := e.slots.Used(); used != 0 {
		t.Fatalf("image-pull backoff holds a VM slot: used=%d", used)
	}
	p, err := e.Get("default", "pull")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase == corev1.PodFailed {
		t.Fatalf("transient pull failed the pod: %s", p.Status.Message)
	}
	if err := e.Create(context.Background(), samplePod("other", "uid-other"), Credentials{}); err != nil {
		t.Fatalf("pod rejected while the only other pod is pulling: %v", err)
	}
	waitPhase(t, e, "default", "other", corev1.PodRunning)
	if used := e.slots.Used(); used != 1 {
		t.Fatalf("running pod slots=%d, want 1", used)
	}
	if _, err := e.Get("default", "pull"); err != nil {
		t.Fatal(err)
	}
}

// uidStartFail fails Start only for one pod UID. The other pod must be
// able to boot while the first one is in start-retry backoff.
type uidStartFail struct {
	*fake.Runtime
	uid string
}

func (r *uidStartFail) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	if spec.ID.UID == r.uid {
		return &uidStartFailMachine{Machine: m}, nil
	}
	return m, nil
}

type uidStartFailMachine struct{ runtime.Machine }

func (m *uidStartFailMachine) Start(context.Context) error {
	return errors.New("injected start failure")
}

func TestBootBackoffDoesNotBlockAnotherPod(t *testing.T) {
	rt := &uidStartFail{Runtime: fake.New(), uid: "uid-boot"}
	e, clk := oneSlotEngine(t, rt)
	if err := e.Create(context.Background(), samplePod("boot", "uid-boot"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	awaitArmed(t, clk)
	if used := e.slots.Used(); used != 0 {
		t.Fatalf("VM-start backoff holds a VM slot: used=%d", used)
	}
	if rt.Running() != 0 {
		t.Fatalf("failed start left %d VMs running", rt.Running())
	}
	if err := e.Create(context.Background(), samplePod("other", "uid-other"), Credentials{}); err != nil {
		t.Fatalf("pod rejected while the only other pod is backing off a start: %v", err)
	}
	waitPhase(t, e, "default", "other", corev1.PodRunning)
	if used := e.slots.Used(); used != 1 {
		t.Fatalf("running pod slots=%d, want 1", used)
	}
	p, err := e.Get("default", "boot")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase == corev1.PodFailed {
		t.Fatalf("transient boot failure failed the pod: %s", p.Status.Message)
	}
}

// blockCreateRuntime parks inside Create until ctx is cancelled or release
// is closed. A warm boot stuck here holds a slot a real pod must reclaim.
type blockCreateRuntime struct {
	*fake.Runtime
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockCreateRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	block := false
	b.once.Do(func() {
		block = true
		close(b.entered)
	})
	if !block {
		return b.Runtime.Create(ctx, spec)
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-b.release:
		return b.Runtime.Create(ctx, spec)
	}
}
