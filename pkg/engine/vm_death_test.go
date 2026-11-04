package engine

import (
	"context"
	"os"
	"path/filepath"
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

func deathEngine(t *testing.T) *Engine {
	t.Helper()
	leakcheck.Check(t)
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	cfg.ProbeInterval = 20 * time.Millisecond
	e := New(cfg, slots, fake.New(), sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)
	return e
}

func podMachine(t *testing.T, e *Engine, key string) runtime.Machine {
	t.Helper()
	e.mu.RLock()
	rec := e.pods[key]
	e.mu.RUnlock()
	if rec == nil {
		t.Fatalf("no record for %s", key)
	}
	rec.mu.Lock()
	m := rec.machine
	rec.mu.Unlock()
	if m == nil {
		t.Fatal("machine missing")
	}
	return m
}

func assertDeadVMReclaimed(t *testing.T, e *Engine, uid, want string) {
	t.Helper()
	p := waitPhase(t, e, "default", "die", corev1.PodFailed)
	if !strings.Contains(p.Status.Message, want) {
		t.Fatalf("status message %q, want it to mention %q", p.Status.Message, want)
	}
	if podReady(p) {
		t.Fatal("dead VM still reported ready")
	}
	if len(p.Status.ContainerStatuses) == 0 || p.Status.ContainerStatuses[0].State.Terminated == nil {
		t.Fatalf("container still running: %+v", p.Status.ContainerStatuses)
	}
	dir := filepath.Join(e.cfg.CacheDir, "pods", uid)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		_, statErr := os.Stat(dir)
		if e.slots.Used() == 0 && os.IsNotExist(statErr) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if used := e.slots.Used(); used != 0 {
		t.Fatalf("slot still held after VM death: %d", used)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("pod dir left behind: %v", err)
	}
	if err := e.Create(context.Background(), samplePod("next", uid+"-next"), Credentials{}); err != nil {
		t.Fatalf("capacity still pinned: %v", err)
	}
	waitPhase(t, e, "default", "next", corev1.PodRunning)
	_ = e.Delete(context.Background(), "default", "next", 0)
}

func TestUnexpectedVMStopFailsPodAndFreesSlot(t *testing.T) {
	e := deathEngine(t)
	if err := e.Create(context.Background(), samplePod("die", "uid-die"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "die", corev1.PodRunning)
	if err := podMachine(t, e, "default/die").Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	assertDeadVMReclaimed(t, e, "uid-die", "Stopped")
}

// statusMachine reports a fixed VM state while the real guest keeps running,
// so a Failed status is not confused with a probe error.
type statusMachine struct {
	runtime.Machine
	state types.VMState
}

func (m statusMachine) Status() types.VMStatus {
	st := m.Machine.Status()
	st.State = m.state
	return st
}

func TestUnexpectedVMFailedStateFailsPodAndFreesSlot(t *testing.T) {
	e := deathEngine(t)
	if err := e.Create(context.Background(), samplePod("die", "uid-failstate"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "die", corev1.PodRunning)
	e.mu.RLock()
	rec := e.pods["default/die"]
	e.mu.RUnlock()
	rec.mu.Lock()
	rec.machine = statusMachine{Machine: rec.machine, state: types.VMFailed}
	rec.mu.Unlock()
	assertDeadVMReclaimed(t, e, "uid-failstate", "Failed")
}
