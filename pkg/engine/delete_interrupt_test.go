package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// stopHoldRuntime blocks Stop while hold is set so a test can read the
// status virtual-kubelet would sync before the pod record is dropped.
// Delete, drain, and eviction all come through Engine.Delete.
type stopHoldRuntime struct {
	runtime.Runtime
	mu      sync.Mutex
	hold    bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *stopHoldRuntime) arm(hold bool) {
	r.mu.Lock()
	r.hold = hold
	r.mu.Unlock()
}

func (r *stopHoldRuntime) holding() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hold
}

func (r *stopHoldRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &stopHoldMachine{Machine: m, rt: r}, nil
}

type stopHoldMachine struct {
	runtime.Machine
	rt *stopHoldRuntime
}

func (m *stopHoldMachine) Stop(ctx context.Context, grace time.Duration) error {
	if m.rt.holding() {
		m.rt.once.Do(func() { close(m.rt.entered) })
		select {
		case <-m.rt.release:
		case <-ctx.Done():
		}
	}
	return m.Machine.Stop(ctx, grace)
}

func interruptEngine(t *testing.T, hold bool) (*Engine, *fake.Runtime, *stopHoldRuntime) {
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
	inner := fake.New()
	rt := &stopHoldRuntime{
		Runtime: inner,
		hold:    hold,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	return e, inner, rt
}

func waitStopEntered(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("stop did not start")
	}
}

func releaseAndClose(t *testing.T, e *Engine, rt *stopHoldRuntime, wg *sync.WaitGroup) func() {
	t.Helper()
	var once sync.Once
	release := func() { once.Do(func() { close(rt.release) }) }
	t.Cleanup(func() {
		release()
		if wg != nil {
			wg.Wait()
		}
		_ = e.Close()
	})
	return release
}

// A running pod that is deleted has not finished. Succeeded with exit 0
// would make a Job count the interrupted run as success. Grace 0 is
// kubelet's SIGKILL (137). A positive grace is the SIGTERM shutdown (143).
// Drain and eviction use the same Delete path.
func TestDeleteOfRunningPodIsFailed(t *testing.T) {
	cases := []struct {
		name  string
		grace int64
		code  int32
		text  string
		drain bool
	}{
		{name: "killed", grace: 0, code: 137, text: "SIGKILL"},
		{name: "terminated", grace: 30, code: 143, text: "SIGTERM"},
		{name: "drained", grace: 30, code: 143, text: "SIGTERM", drain: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _, rt := interruptEngine(t, true)
			var wg sync.WaitGroup
			release := releaseAndClose(t, e, rt, &wg)
			pod := samplePod("run", "uid-interrupt-"+tc.name)
			pod.Spec.RestartPolicy = corev1.RestartPolicyNever
			grace := tc.grace
			pod.Spec.TerminationGracePeriodSeconds = &grace
			if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
				t.Fatal(err)
			}
			waitPhase(t, e, "default", "run", corev1.PodRunning)
			errCh := make(chan error, 1)
			wg.Add(1)
			go func() {
				defer wg.Done()
				if tc.drain {
					errCh <- e.Drain(context.Background())
					return
				}
				errCh <- e.Delete(context.Background(), "default", "run", tc.grace)
			}()
			waitStopEntered(t, rt.entered)
			p, err := e.Get("default", "run")
			if err != nil {
				t.Fatal(err)
			}
			release()
			if err := <-errCh; err != nil {
				t.Fatal(err)
			}
			if p.Status.Phase != corev1.PodFailed {
				t.Fatalf("phase %s reason %q, a pod stopped before its workload exited was reported successful", p.Status.Phase, p.Status.Reason)
			}
			term := terminated(t, p)
			if term.ExitCode != tc.code || term.Reason != "Error" || !strings.Contains(term.Message, tc.text) {
				t.Fatalf("exit %d reason %q message %q, want %d Error containing %s", term.ExitCode, term.Reason, term.Message, tc.code, tc.text)
			}
			if _, err := e.Get("default", "run"); err == nil {
				t.Fatal("pod record survived the stop")
			}
			if used := e.slots.Used(); used != 0 {
				t.Fatalf("slot still held: %d", used)
			}
		})
	}
}

// A guest exit of 0 means the workload finished. Deleting that pod must
// stay Succeeded with that code, not a kill code, or a Job that completed
// during shutdown is counted as a failure.
func TestDeleteAfterGuestExitZeroStaysSucceeded(t *testing.T) {
	e, rt, hold := interruptEngine(t, false)
	var wg sync.WaitGroup
	release := releaseAndClose(t, e, hold, &wg)
	pod := samplePod("job", "uid-delete-after-zero")
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "job", corev1.PodRunning)
	zero := int32(0)
	rt.ReportExit(guest.ExitReport{ExitCode: &zero, Reason: "Completed"})
	waitGuestExit(t, e, "default/job", 0)
	hold.arm(true)
	errCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- e.Delete(context.Background(), "default", "job", 0)
	}()
	waitStopEntered(t, hold.entered)
	p, err := e.Get("default", "job")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("phase %s reason %q, deleting a finished workload discarded exit 0", p.Status.Phase, p.Status.Reason)
	}
	term := terminated(t, p)
	if term.ExitCode != 0 || term.Reason != "Completed" {
		t.Fatalf("exit %d reason %q, want 0 Completed", term.ExitCode, term.Reason)
	}
}

// A pod that already failed keeps the guest code across delete. A grace-0
// delete must not rewrite it to 137.
func TestDeleteKeepsGuestFailureCode(t *testing.T) {
	e, rt, hold := interruptEngine(t, false)
	var wg sync.WaitGroup
	release := releaseAndClose(t, e, hold, &wg)
	pod := samplePod("job", "uid-delete-keeps-code")
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "job", corev1.PodRunning)
	code := int32(9)
	rt.ReportExit(guest.ExitReport{ExitCode: &code, Reason: "Error"})
	waitGuestExit(t, e, "default/job", 9)
	if err := podMachine(t, e, "default/job").Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "job", corev1.PodFailed)
	if got := terminated(t, p).ExitCode; got != 9 {
		t.Fatalf("exit %d before delete, guest code was not published", got)
	}
	waitSlotReleased(t, e, rt)
	hold.arm(true)
	errCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		errCh <- e.Delete(context.Background(), "default", "job", 0)
	}()
	waitStopEntered(t, hold.entered)
	p, err := e.Get("default", "job")
	if err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	term := terminated(t, p)
	if p.Status.Phase != corev1.PodFailed || term.ExitCode != 9 {
		t.Fatalf("phase %s exit %d, delete rewrote a guest failure", p.Status.Phase, term.ExitCode)
	}
}
