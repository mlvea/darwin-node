package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
)

// lateRestartDelay is long enough that Close or Delete returns on today's
// code, and the in-flight call still completes afterwards. vz.Start does not
// look at ctx; the fakes below boot or create even after cancellation.
const lateRestartDelay = 350 * time.Millisecond

// lateStartRuntime's second Start is a liveness restart already inside
// machine.Start. It sleeps, then boots with a live context, the same as
// Virtualization.framework ignoring cancellation.
type lateStartRuntime struct {
	*fake.Runtime
	entered chan struct{}
	once    sync.Once
}

func (r *lateStartRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &lateStartMachine{Machine: m, rt: r}, nil
}

type lateStartMachine struct {
	runtime.Machine
	rt     *lateStartRuntime
	starts atomic.Int32
}

func (m *lateStartMachine) Start(ctx context.Context) error {
	if m.starts.Add(1) >= 2 {
		m.rt.once.Do(func() { close(m.rt.entered) })
		time.Sleep(lateRestartDelay)
		return m.Machine.Start(context.Background())
	}
	return m.Machine.Start(ctx)
}

func livenessPod(name, uid string) *corev1.Pod {
	p := samplePod(name, uid)
	p.Spec.RestartPolicy = corev1.RestartPolicyAlways
	p.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"live"}}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 1,
	}
	return p
}

func failLiveOnce(rt *fake.Runtime, entered chan struct{}) {
	var n atomic.Int32
	var once sync.Once
	rt.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) == 0 || req.Argv[0] != "live" {
			return 0, nil
		}
		if n.Add(1) == 1 {
			once.Do(func() { close(entered) })
			// The probe is in flight across Close. It does not abort.
			time.Sleep(lateRestartDelay)
			return 1, fmt.Errorf("dead")
		}
		return 0, nil
	}
}

func waitEntered(t *testing.T, entered <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not start", what)
	}
}

func settleLateRestart() {
	time.Sleep(lateRestartDelay + 250*time.Millisecond)
}

// TestCloseDuringLivenessRestartDoesNotBootAVM is a restart already inside
// Start while Close runs. The slot is released today, then Start boots.
func TestCloseDuringLivenessRestartDoesNotBootAVM(t *testing.T) {
	entered := make(chan struct{})
	inner := fake.New()
	inner.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "live" {
			return 1, nil
		}
		return 0, nil
	}
	rt := &lateStartRuntime{Runtime: inner, entered: entered}
	e := sidecarEngine(t, rt, sidecar.None{})
	if err := e.Create(context.Background(), livenessPod("lv", "uid-lv-close"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, entered, "liveness restart Start")

	e.Close()
	settleLateRestart()

	if got, slots := rt.Running(), e.slots.Used(); got != 0 || slots != 0 {
		t.Fatalf("Close left running=%d slots=%d; liveness Start booted a VM after the slot was released", got, slots)
	}
	if !e.Draining() {
		t.Fatal("Close must reject further creates")
	}
	if err := e.Create(context.Background(), samplePod("next", "uid-lv-next"), Credentials{}); err == nil {
		t.Fatal("create after Close succeeded")
	}
}

// TestDeleteDuringLivenessRestartDoesNotExceedCap deletes one pod while its
// liveness restart is inside Start, then fills both slots.
func TestDeleteDuringLivenessRestartDoesNotExceedCap(t *testing.T) {
	entered := make(chan struct{})
	inner := fake.New()
	inner.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "live" {
			return 1, nil
		}
		return 0, nil
	}
	rt := &lateStartRuntime{Runtime: inner, entered: entered}
	e := sidecarEngine(t, rt, sidecar.None{})
	if err := e.Create(context.Background(), livenessPod("lv", "uid-lv-del"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, entered, "liveness restart Start")

	if err := e.Delete(context.Background(), "default", "lv", 0); err != nil {
		t.Fatal(err)
	}
	settleLateRestart()
	if got, slots := rt.Running(), e.slots.Used(); got != 0 || slots != 0 {
		t.Fatalf("Delete left running=%d slots=%d; liveness Start booted a VM after the slot was released", got, slots)
	}

	if err := e.Create(context.Background(), samplePod("b", "uid-lv-b"), Credentials{}); err != nil {
		t.Fatalf("first free slot: %v", err)
	}
	if err := e.Create(context.Background(), samplePod("c", "uid-lv-c"), Credentials{}); err != nil {
		t.Fatalf("second free slot: %v", err)
	}
	waitPhase(t, e, "default", "b", corev1.PodRunning)
	waitPhase(t, e, "default", "c", corev1.PodRunning)
	err := e.Create(context.Background(), samplePod("d", "uid-lv-d"), Credentials{})
	if !errors.Is(err, capacity.ErrVMCapacityExhausted) {
		if err == nil {
			_ = e.Delete(context.Background(), "default", "d", 0)
		}
		t.Fatalf("cap after delete during liveness restart: %v", err)
	}
	if got := rt.Running(); got != 2 {
		t.Fatalf("running VMs = %d with 2 slots held", got)
	}
}

// TestCloseDuringLivenessProbeDoesNotStartTheRestart is the probe that is
// still running when Close cancels the pod. When it returns, the watch must
// not Start the VM.
func TestCloseDuringLivenessProbeDoesNotStartTheRestart(t *testing.T) {
	entered := make(chan struct{})
	rt := fake.New()
	failLiveOnce(rt, entered)
	e := sidecarEngine(t, rt, sidecar.None{})
	if err := e.Create(context.Background(), livenessPod("lv", "uid-lv-probe"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, entered, "liveness probe")
	starts := rt.Starts()

	e.Close()
	settleLateRestart()

	if got, slots := rt.Running(), e.slots.Used(); got != 0 || slots != 0 {
		t.Fatalf("Close left running=%d slots=%d after an in-flight liveness probe", got, slots)
	}
	if rt.Starts() != starts {
		t.Fatalf("starts %d -> %d; the probe failure restarted the VM after Close", starts, rt.Starts())
	}
}

// lateSidecar's second Create is the crash restart. It sleeps, then leaves
// the container running even if the pod context is already cancelled.
type lateSidecar struct {
	sidecar.None
	mu      sync.Mutex
	byName  map[string]sidecar.Status
	creates int
	entered chan struct{}
	once    sync.Once
}

func newLateSidecar() *lateSidecar {
	return &lateSidecar{byName: map[string]sidecar.Status{}, entered: make(chan struct{})}
}

func (s *lateSidecar) Create(_ context.Context, _ *corev1.Pod, c corev1.Container, _ string) error {
	s.mu.Lock()
	s.creates++
	n := s.creates
	s.mu.Unlock()
	if n >= 2 {
		s.once.Do(func() { close(s.entered) })
		time.Sleep(lateRestartDelay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := sidecar.Status{
		Name:      c.Name,
		ID:        fmt.Sprintf("mem://%s-%d", c.Name, n),
		StartedAt: time.Now(),
	}
	if n == 1 {
		st.State = "terminated"
		st.ExitCode = 1
		st.FinishedAt = st.StartedAt
	} else {
		st.State = "running"
	}
	s.byName[c.Name] = st
	return nil
}

func (s *lateSidecar) RemovePod(context.Context, string, string, int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byName = map[string]sidecar.Status{}
	return nil
}

func (s *lateSidecar) List(context.Context, string, string) ([]sidecar.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]sidecar.Status, 0, len(s.byName))
	for _, st := range s.byName {
		out = append(out, st)
	}
	return out, nil
}

func (s *lateSidecar) running() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, st := range s.byName {
		if st.State == "running" {
			n++
		}
	}
	return n
}

func TestCloseDuringSidecarRestartDoesNotLeaveAContainer(t *testing.T) {
	rt := fake.New()
	sc := newLateSidecar()
	e := sidecarEngine(t, rt, sc)
	if err := e.Create(context.Background(), hybridPod("hy", "uid-hy-close", corev1.RestartPolicyAlways), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, sc.entered, "sidecar restart Create")

	e.Close()
	settleLateRestart()

	if n := sc.running(); n != 0 {
		t.Fatalf("sidecar Create left %d running container(s) after Close removed the pod", n)
	}
	if got, slots := rt.Running(), e.slots.Used(); got != 0 || slots != 0 {
		t.Fatalf("running=%d slots=%d after Close during sidecar restart", got, slots)
	}
}

func TestDeleteDuringSidecarRestartDoesNotLeaveAContainer(t *testing.T) {
	rt := fake.New()
	sc := newLateSidecar()
	e := sidecarEngine(t, rt, sc)
	if err := e.Create(context.Background(), hybridPod("hy", "uid-hy-del", corev1.RestartPolicyAlways), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, sc.entered, "sidecar restart Create")

	if err := e.Delete(context.Background(), "default", "hy", 0); err != nil {
		t.Fatal(err)
	}
	settleLateRestart()

	if n := sc.running(); n != 0 {
		t.Fatalf("sidecar Create left %d running container(s) after Delete", n)
	}
	if got, slots := rt.Running(), e.slots.Used(); got != 0 || slots != 0 {
		t.Fatalf("running=%d slots=%d after Delete during sidecar restart", got, slots)
	}
	if err := e.Create(context.Background(), samplePod("next", "uid-hy-next"), Credentials{}); err != nil {
		t.Fatalf("slot not reusable: %v", err)
	}
	waitPhase(t, e, "default", "next", corev1.PodRunning)
	if rt.Running() != 1 || e.slots.Used() != 1 {
		t.Fatalf("running=%d slots=%d", rt.Running(), e.slots.Used())
	}
}

// blockingProbeSidecar holds the startup exec until the pod context ends,
// then returns the cancellation. Close has to unblock it and leave no VM.
type blockingProbeSidecar struct {
	sidecar.None
	mu      sync.Mutex
	st      sidecar.Status
	entered chan struct{}
	once    sync.Once
	execs   atomic.Int32
}

func (s *blockingProbeSidecar) Create(_ context.Context, _ *corev1.Pod, c corev1.Container, _ string) error {
	s.mu.Lock()
	s.st = sidecar.Status{Name: c.Name, ID: "mem://" + c.Name, State: "running", StartedAt: time.Now()}
	s.mu.Unlock()
	return nil
}

func (s *blockingProbeSidecar) RemovePod(context.Context, string, string, int64) error {
	s.mu.Lock()
	s.st = sidecar.Status{}
	s.mu.Unlock()
	return nil
}

func (s *blockingProbeSidecar) List(context.Context, string, string) ([]sidecar.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st.Name == "" {
		return nil, nil
	}
	return []sidecar.Status{s.st}, nil
}

func (s *blockingProbeSidecar) Exec(ctx context.Context, _, _, _ string, cmd []string, _ api.AttachIO) error {
	s.execs.Add(1)
	if len(cmd) > 0 && cmd[0] == "sidecar-boot" {
		s.once.Do(func() { close(s.entered) })
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func TestCloseDuringSidecarStartupProbeDoesNotRestartVM(t *testing.T) {
	rt := fake.New()
	sc := &blockingProbeSidecar{entered: make(chan struct{})}
	e := sidecarEngine(t, rt, sc)
	pod := hybridPod("hy", "uid-hy-probe", corev1.RestartPolicyAlways)
	// Sidecar probes no longer block the watch, so a due failing VM liveness
	// would restart on its own. The delay keeps this test on the
	// cancellation path.
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler:        corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"live"}}},
		FailureThreshold:    1,
		TimeoutSeconds:      1,
		InitialDelaySeconds: 3600,
	}
	pod.Spec.Containers[1].StartupProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"sidecar-boot"}}},
		FailureThreshold: 1,
		TimeoutSeconds:   5,
	}
	var lives atomic.Int32
	rt.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "live" {
			lives.Add(1)
			return 1, nil
		}
		return 0, nil
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitEntered(t, sc.entered, "sidecar startup probe")
	starts := rt.Starts()

	e.Close()

	if got, slots := rt.Running(), e.slots.Used(); got != 0 || slots != 0 {
		t.Fatalf("running=%d slots=%d after Close during a sidecar startup probe", got, slots)
	}
	if rt.Starts() != starts {
		t.Fatalf("starts %d -> %d; startup-probe cancellation restarted the VM", starts, rt.Starts())
	}
	if sc.execs.Load() == 0 {
		t.Fatal("startup probe did not run")
	}
}

func TestCloseDuringCrashLoopBackoffDoesNotStartSidecar(t *testing.T) {
	rt := fake.New()
	sc := newRestartingSidecar()
	sc.exitAlways["log"] = 1
	e := sidecarEngine(t, rt, sc)
	if err := e.Create(context.Background(), hybridPod("hy", "uid-hy-bo", corev1.RestartPolicyAlways), Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "hy")
		side := (*corev1.ContainerStatus)(nil)
		if err == nil {
			side = containerStatus(p, "log")
		}
		if err == nil && p.Status.Phase == corev1.PodRunning && sc.createsOf("log") == 2 &&
			side != nil && side.State.Waiting != nil && side.State.Waiting.Reason == "CrashLoopBackOff" {
			break
		}
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("crash loop failed the pod: %s", p.Status.Message)
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sc.createsOf("log") != 2 {
		t.Fatalf("creates=%d, backoff was not armed", sc.createsOf("log"))
	}
	n := sc.createsOf("log")
	e.Close()
	// Several probe intervals. A watch that survived Close would be ticking.
	// The backoff itself is not due yet; an extra Create means the watch
	// restarted the container anyway, or a timer fired.
	time.Sleep(200 * time.Millisecond)
	if got := sc.createsOf("log"); got != n {
		t.Fatalf("creates %d -> %d after Close during CrashLoopBackOff", n, got)
	}
	if rt.Running() != 0 || e.slots.Used() != 0 {
		t.Fatalf("running=%d slots=%d", rt.Running(), e.slots.Used())
	}
}

// Delete during CrashLoopBackOff has to return without waiting out the
// backoff. The slot is the pod's, and kubelet frees it when the pod is
// deleted. A backoff that swallowed Delete would leave the slot held.
func TestDeleteDuringCrashLoopBackoffFreesTheSlot(t *testing.T) {
	rt := fake.New()
	sc := newRestartingSidecar()
	sc.exitAlways["log"] = 1
	e := sidecarEngine(t, rt, sc)
	if err := e.Create(context.Background(), hybridPod("hy", "uid-hy-del", corev1.RestartPolicyAlways), Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "hy")
		side := (*corev1.ContainerStatus)(nil)
		if err == nil {
			side = containerStatus(p, "log")
		}
		if err == nil && p.Status.Phase == corev1.PodRunning && sc.createsOf("log") == 2 &&
			side != nil && side.State.Waiting != nil && side.State.Waiting.Reason == "CrashLoopBackOff" {
			break
		}
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("crash loop failed the pod: %s", p.Status.Message)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if sc.createsOf("log") != 2 {
		t.Fatalf("creates=%d, backoff was not armed", sc.createsOf("log"))
	}
	deleted := make(chan error, 1)
	go func() { deleted <- e.Delete(context.Background(), "default", "hy", 0) }()
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Delete did not return during CrashLoopBackOff")
	}
	if rt.Running() != 0 || e.slots.Used() != 0 {
		t.Fatalf("running=%d slots=%d, backoff kept the slot after Delete", rt.Running(), e.slots.Used())
	}
	if err := e.Create(context.Background(), samplePod("next", "uid-hy-next"), Credentials{}); err != nil {
		t.Fatalf("next pod after delete: %v", err)
	}
	waitPhase(t, e, "default", "next", corev1.PodRunning)
}

// A crash-looping sidecar keeps the pod Running and holds one of the two
// VM slots. That is the kubelet rule: CrashLoopBackOff delays the next
// start and does not evict the pod. The other slot still admits a pod.
func TestCrashLoopingSidecarKeepsItsVMSlot(t *testing.T) {
	rt := fake.New()
	sc := newRestartingSidecar()
	sc.exitAlways["log"] = 1
	e := sidecarEngine(t, rt, sc)
	if err := e.Create(context.Background(), hybridPod("hy", "uid-hy-slot", corev1.RestartPolicyAlways), Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var got *corev1.Pod
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "hy")
		if err == nil && p.Status.Phase == corev1.PodFailed {
			t.Fatalf("crash loop failed the pod: %s", p.Status.Message)
		}
		side := (*corev1.ContainerStatus)(nil)
		if err == nil {
			side = containerStatus(p, "log")
		}
		if err == nil && p.Status.Phase == corev1.PodRunning && !podReady(p) &&
			side != nil && side.State.Waiting != nil && side.State.Waiting.Reason == "CrashLoopBackOff" &&
			sc.createsOf("log") == 2 {
			got = p
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got == nil {
		p, _ := e.Get("default", "hy")
		t.Fatalf("sidecar was not CrashLoopBackOff: %+v creates=%d", containerStatus(p, "log"), sc.createsOf("log"))
	}
	side := containerStatus(got, "log")
	if side.Ready || side.RestartCount != 1 || side.State.Terminated != nil {
		t.Fatalf("status during backoff %+v", side)
	}
	if side.State.Waiting.Message == "" || side.LastTerminationState.Terminated == nil || side.LastTerminationState.Terminated.ExitCode != 1 {
		t.Fatalf("backoff status %+v", side)
	}
	if e.slots.Used() != 1 || rt.Running() != 1 || got.Status.Phase != corev1.PodRunning {
		t.Fatalf("phase=%s slots=%d running=%d, crash loop must keep the VM slot", got.Status.Phase, e.slots.Used(), rt.Running())
	}
	if err := e.Create(context.Background(), samplePod("other", "uid-hy-other"), Credentials{}); err != nil {
		t.Fatalf("second slot: %v", err)
	}
	waitPhase(t, e, "default", "other", corev1.PodRunning)
	err := e.Create(context.Background(), samplePod("over", "uid-hy-over"), Credentials{})
	if !errors.Is(err, capacity.ErrVMCapacityExhausted) {
		if err == nil {
			_ = e.Delete(context.Background(), "default", "over", 0)
		}
		t.Fatalf("cap while a crash-looping sidecar holds a slot: %v", err)
	}
	if e.slots.Used() != 2 || rt.Running() != 2 {
		t.Fatalf("slots=%d running=%d", e.slots.Used(), rt.Running())
	}
	p, err := e.Get("default", "hy")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase != corev1.PodRunning {
		t.Fatalf("phase %s, admitting another pod failed the crash-looping one", p.Status.Phase)
	}
}
