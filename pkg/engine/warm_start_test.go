package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// startFailRuntime launches the VM, then fails Start for warm-pool names.
// Virtualization.framework opens the vsock listener before vm.Start, so a
// Start error can leave a live machine. The slot table must not drop that
// machine.
type startFailRuntime struct {
	runtime.Runtime
}

func (r *startFailRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &startFailMachine{Machine: m}, nil
}

type startFailMachine struct {
	runtime.Machine
}

func (m *startFailMachine) Start(ctx context.Context) error {
	if err := m.Machine.Start(ctx); err != nil {
		return err
	}
	if strings.HasPrefix(m.ID().Name, "darwin-warm-") {
		return errors.New("start failed after the VM was launched")
	}
	return nil
}

func TestWarmStartErrorDoesNotLeaveVMPastCap(t *testing.T) {
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
	inner := fake.New()
	e := New(cfg, slots, &startFailRuntime{Runtime: inner}, sidecar.None{}, event.Nop{}, "10.0.0.1")
	defer e.Close()

	if err := e.bootWarm(context.Background(), warmImg); err == nil {
		t.Fatal("warm start must fail")
	}
	for _, pod := range []struct{ name, uid string }{{"a", "uid-a"}, {"b", "uid-b"}} {
		if err := e.Create(context.Background(), samplePod(pod.name, pod.uid), Credentials{}); err != nil {
			t.Fatal(err)
		}
		waitPhase(t, e, "default", pod.name, corev1.PodRunning)
	}
	if n, used := inner.Running(), e.slots.Used(); n != 2 || used != 2 {
		t.Fatalf("running VMs = %d with %d slots held; warm start failure leaked a VM past the cap", n, used)
	}
}
