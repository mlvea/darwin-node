package engine

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
)

// slowSidecar holds one exec until the probe context ends. done becomes
// true only after that wait returns, so a test can see that the probe is
// still in flight.
type slowSidecar struct {
	sidecar.None
	mu      sync.Mutex
	st      sidecar.Status
	entered chan struct{}
	once    sync.Once
	done    atomic.Bool
}

func (s *slowSidecar) Create(_ context.Context, _ *corev1.Pod, c corev1.Container, _ string) error {
	s.mu.Lock()
	s.st = sidecar.Status{Name: c.Name, ID: "mem://" + c.Name, State: "running", StartedAt: time.Now()}
	s.mu.Unlock()
	return nil
}

func (s *slowSidecar) List(context.Context, string, string) ([]sidecar.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Name == "" {
		return nil, nil
	}
	return []sidecar.Status{s.st}, nil
}

func (s *slowSidecar) Exec(ctx context.Context, _, _, _ string, _ []string, _ api.AttachIO) error {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	s.done.Store(true)
	return ctx.Err()
}

// A sidecar probe runs on the watch, ahead of the VM probes. One slow
// sidecar sample must not hold the VM readiness sample for timeoutSeconds.
func TestSlowSidecarProbeDoesNotDelayVMReadiness(t *testing.T) {
	rt := fake.New()
	sc := &slowSidecar{entered: make(chan struct{})}
	e := sidecarEngine(t, rt, sc)
	var vmExecs atomic.Int32
	rt.ExecFn = func(_ context.Context, req guest.ExecReq, _ io.Reader, _, _ io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "vm-ready" {
			vmExecs.Add(1)
			return 0, nil
		}
		return 0, nil
	}
	pod := hybridPod("side", "uid-slow-side", corev1.RestartPolicyAlways)
	pod.Spec.Containers[0].ReadinessProbe = execProbe("vm-ready", 1, 1, 3, 1)
	pod.Spec.Containers[1].ReadinessProbe = execProbe("side-slow", 1, 30, 3, 1)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sc.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("sidecar probe did not start")
	}
	deadline := time.Now().Add(time.Second)
	for vmExecs.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if sc.done.Load() {
		t.Fatal("sidecar probe returned before the VM sample; the watch was not blocked by it")
	}
	if vmExecs.Load() == 0 {
		t.Fatalf("slow sidecar probe delayed the VM readiness probe: vmExecs=0 while the sidecar probe is still blocked (timeoutSeconds=30)")
	}
}
