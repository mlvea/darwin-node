package engine

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/node"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/supervisor"

	corev1 "k8s.io/api/core/v1"
)

func startSupMajor(t *testing.T, dir string, major int) *runningSup {
	t.Helper()
	rt := fake.New()
	rt.SetBridgeGuest(true)
	ctx, cancel := context.WithCancel(context.Background())
	srv := &supervisor.Server{
		Slot:               0,
		RunDir:             dir,
		RT:                 rt,
		BinaryVersion:      "sv-test",
		Deadman:            time.Hour,
		AdvertiseWireMajor: major,
	}
	s := &runningSup{rt: rt, srv: srv, cancel: cancel, done: make(chan struct{})}
	var serveErr error
	go func() {
		serveErr = srv.Serve(ctx)
		close(s.done)
	}()
	path := supervisor.SocketPath(dir, 0)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			t.Cleanup(func() {
				cancel()
				select {
				case <-s.done:
				case <-time.After(5 * time.Second):
					t.Errorf("supervisor did not exit: %v", serveErr)
				}
			})
			return s
		}
		select {
		case <-s.done:
			t.Fatalf("supervisor exited: %v", serveErr)
		default:
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	t.Fatalf("socket %s did not appear", path)
	return nil
}

func TestWireMajorMismatchHoldsBudgetAndCordons(t *testing.T) {
	for _, meter := range []bool{false, true} {
		meter := meter
		t.Run(map[bool]string{false: "direct", true: "meter"}[meter], func(t *testing.T) {
			leakcheck.Check(t)
			dir := shortRunDir(t)
			sup := startSupMajor(t, dir, 2)
			cache := t.TempDir()
			e1, _ := supEngine(t, cache, dir, false)
			if err := e1.Create(context.Background(), supPod("job", "uid-job"), Credentials{}); err != nil {
				t.Fatal(err)
			}
			waitPhase(t, e1, "default", "job", corev1.PodRunning)
			if sup.rt.Starts() != 1 || sup.rt.Running() != 1 {
				t.Fatalf("starts %d running %d", sup.rt.Starts(), sup.rt.Running())
			}
			crashEngine(t, e1)

			e2, inner := supEngine(t, cache, dir, meter)
			t.Cleanup(func() { _ = e2.Close() })
			if e2.slots.Used() != 1 {
				t.Fatalf("slots used %d, mismatch was not counted", e2.slots.Used())
			}
			if sup.rt.Running() != 1 || sup.rt.Starts() != 1 {
				t.Fatalf("mismatch stopped the VM: starts %d running %d", sup.rt.Starts(), sup.rt.Running())
			}
			skew := e2.SupervisorSkew()
			if skew == nil {
				t.Fatal("no cordon")
			}
			n := &corev1.Node{}
			skew.Apply(n)
			if !n.Spec.Unschedulable {
				t.Fatal("node stayed schedulable")
			}
			found := false
			for _, c := range n.Status.Conditions {
				if c.Type == node.ConditionSupervisorSkew && c.Reason == node.ReasonSupervisorSkew {
					found = true
				}
			}
			if !found {
				t.Fatalf("conditions %+v", n.Status.Conditions)
			}
			if err := e2.Create(context.Background(), supPod("job", "uid-job"), Credentials{}); err != nil {
				t.Fatal(err)
			}
			if sup.rt.Starts() != 1 {
				t.Fatalf("create booted another VM: starts %d", sup.rt.Starts())
			}
			p, err := e2.Get("default", "job")
			if err != nil {
				t.Fatal(err)
			}
			if p.Status.Phase != corev1.PodPending || p.Status.Reason != skewPodReason {
				t.Fatalf("placeholder phase %s reason %s", p.Status.Phase, p.Status.Reason)
			}
			time.Sleep(100 * time.Millisecond)
			if sup.rt.Running() != 1 {
				t.Fatal("hold stopped the VM")
			}
			sup.cancel()
			select {
			case <-sup.done:
			case <-time.After(5 * time.Second):
				t.Fatal("supervisor did not exit")
			}
			inner.WaitSkew(3 * time.Second)
			if e2.slots.Used() != 0 {
				t.Fatalf("slot still used %d", e2.slots.Used())
			}
			if inner.HeldSlots() != 0 {
				t.Fatalf("supervisor slot still held")
			}
			cleared := &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}}
			skew.Apply(cleared)
			if cleared.Spec.Unschedulable {
				t.Fatal("node stayed unschedulable after the supervisor exited")
			}
			p, err = e2.Get("default", "job")
			if err != nil {
				t.Fatal(err)
			}
			if p.Status.Phase != corev1.PodFailed || p.Status.Reason != "SupervisorExited" {
				t.Fatalf("after exit phase %s reason %s", p.Status.Phase, p.Status.Reason)
			}
		})
	}
}

func TestRecoveredWorkloadRunsOnce(t *testing.T) {
	leakcheck.Check(t)
	dir := shortRunDir(t)
	sup := startSup(t, dir, 0, true)
	var calls atomic.Int32
	sup.rt.WorkloadFn = func(context.Context, guest.WorkloadReq) error {
		calls.Add(1)
		return nil
	}
	cache := t.TempDir()
	e1, _ := supEngine(t, cache, dir, false)
	if err := e1.Create(context.Background(), supPod("job", "uid-job"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e1, "default", "job", corev1.PodRunning)
	if calls.Load() != 0 {
		t.Fatalf("workload calls %d before a command existed", calls.Load())
	}
	crashEngine(t, e1)

	e2, _ := supEngine(t, cache, dir, true)
	t.Cleanup(func() { _ = e2.Close() })
	pod := replaySupPod("job", "uid-job")
	pod.Spec.Containers[0].Command = []string{"/usr/bin/true"}
	if err := e2.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e2, "default", "job", corev1.PodRunning)
	if calls.Load() != 1 {
		t.Fatalf("workload calls %d, want 1", calls.Load())
	}
}

func TestRecoveredWorkloadDoesNotRunTwice(t *testing.T) {
	leakcheck.Check(t)
	dir := shortRunDir(t)
	sup := startSup(t, dir, 0, true)
	var calls atomic.Int32
	sup.rt.WorkloadFn = func(context.Context, guest.WorkloadReq) error {
		calls.Add(1)
		return nil
	}
	cache := t.TempDir()
	e1, _ := supEngine(t, cache, dir, false)
	pod := supPod("job", "uid-job")
	pod.Spec.Containers[0].Command = []string{"/usr/bin/true"}
	if err := e1.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e1, "default", "job", corev1.PodRunning)
	if calls.Load() != 1 {
		t.Fatalf("first start calls %d", calls.Load())
	}
	crashEngine(t, e1)

	e2, _ := supEngine(t, cache, dir, true)
	t.Cleanup(func() { _ = e2.Close() })
	again := replaySupPod("job", "uid-job")
	again.Spec.Containers[0].Command = []string{"/usr/bin/true"}
	if err := e2.Create(context.Background(), again, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e2, "default", "job", corev1.PodRunning)
	if calls.Load() != 1 {
		t.Fatalf("workload calls %d, want 1", calls.Load())
	}
}

func TestRecoveredWorkloadExitCodeIsPodStatus(t *testing.T) {
	for _, code := range []int32{0, 9} {
		code := code
		t.Run(map[int32]string{0: "zero", 9: "nine"}[code], func(t *testing.T) {
			leakcheck.Check(t)
			dir := shortRunDir(t)
			sup := startSup(t, dir, 0, true)
			var calls atomic.Int32
			sup.rt.WorkloadFn = func(context.Context, guest.WorkloadReq) error {
				calls.Add(1)
				return nil
			}
			cache := t.TempDir()
			e1, _ := supEngine(t, cache, dir, false)
			if err := e1.Create(context.Background(), supPod("job", "uid-job"), Credentials{}); err != nil {
				t.Fatal(err)
			}
			waitPhase(t, e1, "default", "job", corev1.PodRunning)
			c, err := supervisor.Dial(context.Background(), supervisor.SocketPath(dir, 0), "agent-test", 0)
			if err != nil {
				t.Fatal(err)
			}
			exit := code
			if err := c.NoteWorkload(context.Background(), supervisor.WorkloadNote{Started: true, Exited: true, ExitCode: &exit}); err != nil {
				t.Fatal(err)
			}
			_ = c.Close()
			crashEngine(t, e1)

			e2, _ := supEngine(t, cache, dir, true)
			t.Cleanup(func() { _ = e2.Close() })
			pod := replaySupPod("job", "uid-job")
			pod.Spec.Containers[0].Command = []string{"/usr/bin/true"}
			if err := e2.Create(context.Background(), pod, Credentials{}); err != nil {
				t.Fatal(err)
			}
			want := corev1.PodFailed
			if code == 0 {
				want = corev1.PodSucceeded
			}
			got := waitPhase(t, e2, "default", "job", want)
			if calls.Load() != 0 {
				t.Fatalf("workload calls %d after an exited handshake", calls.Load())
			}
			cs := containerStatus(got, "macos")
			if cs == nil || cs.State.Terminated == nil || cs.State.Terminated.ExitCode != code {
				t.Fatalf("container %+v", cs)
			}
		})
	}
}
