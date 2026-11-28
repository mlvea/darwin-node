package engine

import (
	"context"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// A restart stops the VM before it starts it again. The probe watch can
// observe that stop on its first pass and fail the pod. The restart owns
// the stop, so the watch leaves the phase alone.
func TestRestartStopIsNotTreatedAsVMDeath(t *testing.T) {
	e := testEngine(t)
	m, err := fake.New().Create(context.Background(), types.VMSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(context.Background(), time.Second); err != nil {
		t.Fatal(err)
	}
	rec := &podRecord{
		pod:     samplePod("rs", "uid-guard"),
		phase:   corev1.PodRunning,
		machine: m,
	}
	markRestarting(rec)
	defer clearRestarting(rec)
	restarted, keep := e.noteVMDeath(context.Background(), rec)
	if restarted || !keep {
		t.Fatalf("restart stop was treated as death: restarted=%v keep=%v", restarted, keep)
	}
	rec.mu.Lock()
	phase := rec.phase
	rec.mu.Unlock()
	if phase != corev1.PodRunning {
		t.Fatalf("phase %s", phase)
	}
}
