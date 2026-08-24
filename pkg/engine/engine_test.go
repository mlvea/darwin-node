package engine

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

func testEngine(t *testing.T) *Engine {
	t.Helper()
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	return New(cfg, slots, fake.New(), sidecar.None{}, event.Nop{}, "10.0.0.1")
}

func samplePod(name, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: k8stypes.UID(uid)},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "macos", Image: "local/macos:test"}},
		},
	}
}

func TestCreateRunningAndThirdFails(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	if err := e.Create(ctx, samplePod("a", "uid-a"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	if err := e.Create(ctx, samplePod("b", "uid-b"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	err := e.Create(ctx, samplePod("c", "uid-c"), Credentials{})
	if !errors.Is(err, capacity.ErrVMCapacityExhausted) {
		t.Fatalf("third pod: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "a")
		if err == nil && p.Status.Phase == corev1.PodRunning && podReady(p) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	p, err := e.Get("default", "a")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase != corev1.PodRunning {
		t.Fatalf("phase %s msg=%s", p.Status.Phase, p.Status.Message)
	}
	if !podReady(p) {
		t.Fatalf("not ready: %+v", p.Status.Conditions)
	}
	if p.Status.PodIP == "" {
		t.Fatal("missing podIP")
	}

	if err := e.Delete(ctx, "default", "a", 0); err != nil {
		t.Fatal(err)
	}
	if err := e.Create(ctx, samplePod("c", "uid-c"), Credentials{}); err != nil {
		t.Fatalf("create after delete: %v", err)
	}
}

func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func TestExecAndLogs(t *testing.T) {
	e := testEngine(t)
	ctx := context.Background()
	if err := e.Create(ctx, samplePod("a", "uid-a"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "a")
		if err == nil && p.Status.Phase == corev1.PodRunning {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := e.ExecInVM(ctx, "default", "a", []string{"true"}, nil); err != nil {
		t.Fatal(err)
	}
	rc, err := e.LogsVM(ctx, "default", "a", api.ContainerLogOpts{Tail: 10})
	if err != nil {
		t.Fatal(err)
	}
	_ = rc.Close()
}

func TestHybridSidecar(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	e := New(cfg, slots, fake.New(), sidecar.NewMemory(), event.Nop{}, "10.0.0.1")
	pod := samplePod("hy", "uid-hy")
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "log", Image: "busybox"})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "hy")
		if err == nil && p.Status.Phase == corev1.PodRunning && len(p.Status.ContainerStatuses) == 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p, _ := e.Get("default", "hy")
	t.Fatalf("hybrid: %+v", p.Status)
}

func waitPhase(t *testing.T, e *Engine, ns, name string, phase corev1.PodPhase) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var p *corev1.Pod
	for time.Now().Before(deadline) {
		got, err := e.Get(ns, name)
		if err == nil && got.Status.Phase == phase {
			return got
		}
		p = got
		time.Sleep(15 * time.Millisecond)
	}
	if p == nil {
		t.Fatalf("pod %s/%s missing, want phase %s", ns, name, phase)
	}
	t.Fatalf("pod %s/%s phase %s msg=%s want %s", ns, name, p.Status.Phase, p.Status.Message, phase)
	return p
}

func TestFailStopsMachineAndFreesSlot(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	rt := fake.New()
	rt.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "false" {
			return 1, nil
		}
		return 0, nil
	}
	e := New(cfg, slots, rt, sidecar.NewMemory(), event.Nop{}, "10.0.0.1")
	pod := samplePod("ps", "uid-fail")
	pod.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{
		PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"false"}}},
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "ps", corev1.PodFailed)
	_ = p
	e.mu.RLock()
	rec := e.pods["default/ps"]
	e.mu.RUnlock()
	if rec == nil {
		t.Fatal("record missing")
	}
	rec.mu.Lock()
	if rec.machine == nil {
		rec.mu.Unlock()
		t.Fatal("machine missing after fail")
	}
	st := rec.machine.Status()
	rec.mu.Unlock()
	if st.State != types.VMStopped {
		t.Fatalf("machine state %s, want Stopped", st.State)
	}
	if e.slots.Used() != 0 {
		t.Fatalf("slot leak: used=%d", e.slots.Used())
	}
	if err := e.Create(context.Background(), samplePod("next", "uid-next"), Credentials{}); err != nil {
		t.Fatalf("slot should be free: %v", err)
	}
	_ = e.Delete(context.Background(), "default", "ps", 0)
	_ = e.Delete(context.Background(), "default", "next", 0)
}

func TestLivenessRestartsThenFailsClosed(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	cfg.ProbeInterval = 20 * time.Millisecond
	rt := fake.New()
	rt.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "dead" {
			return 1, nil
		}
		return 0, nil
	}
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	pod := samplePod("lv", "uid-lv")
	pod.Spec.RestartPolicy = corev1.RestartPolicyAlways
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"dead"}}},
		PeriodSeconds:    1,
		FailureThreshold: 1,
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	_ = waitPhase(t, e, "default", "lv", corev1.PodFailed)
	if rt.Starts() < 2 {
		t.Fatalf("expected VM restart, starts=%d", rt.Starts())
	}
	_ = e.Delete(context.Background(), "default", "lv", 0)
}

func TestFirstReadinessProbeNotDelayed(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	cfg.ProbeInterval = 10 * time.Second
	rt := fake.New()
	rt.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "unready" {
			return 1, nil
		}
		return 0, nil
	}
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	pod := samplePod("rd", "uid-rd")
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler:  corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"unready"}}},
		PeriodSeconds: 30,
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "rd")
		if err == nil && p.Status.Phase == corev1.PodRunning && !podReady(p) {
			_ = e.Delete(context.Background(), "default", "rd", 0)
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ := e.Get("default", "rd")
	_ = e.Delete(context.Background(), "default", "rd", 0)
	t.Fatalf("readiness should fail immediately, not after ProbeInterval: ready=%v phase=%s", podReady(p), p.Status.Phase)
}

func TestInitContainersRun(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	mem := sidecar.NewMemory()
	e := New(cfg, slots, fake.New(), mem, event.Nop{}, "10.0.0.1")
	pod := samplePod("in", "uid-in")
	pod.Spec.InitContainers = []corev1.Container{{Name: "setup", Image: "busybox"}}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "in", corev1.PodRunning)
	found := false
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodInitialized && c.Status == corev1.ConditionTrue {
			found = true
		}
	}
	if !found {
		t.Fatalf("Initialized: %+v", p.Status.Conditions)
	}
	st, err := mem.Get(context.Background(), "default", "in", "setup")
	_ = e.Delete(context.Background(), "default", "in", 0)
	if err != nil || st.State != "terminated" {
		t.Fatalf("init sidecar %+v %v", st, err)
	}
}

type pollSidecar struct {
	sidecar.None
	mu      sync.Mutex
	gets    int
	created time.Time
	delay   time.Duration
	failN   int
}

func (p *pollSidecar) Create(_ context.Context, _ *corev1.Pod, _ corev1.Container, _ string) error {
	p.mu.Lock()
	p.created = time.Now()
	p.mu.Unlock()
	return nil
}

func (p *pollSidecar) Get(_ context.Context, _, _, container string) (sidecar.Status, error) {
	p.mu.Lock()
	p.gets++
	n := p.gets
	created := p.created
	p.mu.Unlock()
	if p.failN > 0 {
		return sidecar.Status{}, errors.New("docker down")
	}
	if p.delay > 0 && time.Since(created) < p.delay {
		return sidecar.Status{Name: container, State: "running"}, nil
	}
	_ = n
	return sidecar.Status{Name: container, State: "terminated", ExitCode: 0}, nil
}

func TestWaitSidecarExitBackoff(t *testing.T) {
	sc := &pollSidecar{delay: 400 * time.Millisecond}
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, fake.New(), sc, event.Nop{}, "10.0.0.1")
	pod := samplePod("inb", "uid-inb")
	pod.Spec.InitContainers = []corev1.Container{{Name: "setup", Image: "busybox"}}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	_ = waitPhase(t, e, "default", "inb", corev1.PodRunning)
	_ = e.Delete(context.Background(), "default", "inb", 0)
	sc.mu.Lock()
	gets := sc.gets
	sc.mu.Unlock()
	if gets > 15 {
		t.Fatalf("poll storm: Get called %d times for 400ms init", gets)
	}
	if gets < 1 {
		t.Fatal("expected at least one Get")
	}
}

func TestWaitSidecarExitDockerDown(t *testing.T) {
	sc := &pollSidecar{failN: 1}
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 2 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, fake.New(), sc, event.Nop{}, "10.0.0.1")
	pod := samplePod("ind", "uid-ind")
	pod.Spec.InitContainers = []corev1.Container{{Name: "setup", Image: "busybox"}}
	start := time.Now()
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "ind", corev1.PodFailed)
	_ = e.Delete(context.Background(), "default", "ind", 0)
	if time.Since(start) > 2*time.Second {
		t.Fatalf("error did not surface quickly: %s msg=%s", time.Since(start), p.Status.Message)
	}
	sc.mu.Lock()
	gets := sc.gets
	sc.mu.Unlock()
	if gets > 5 {
		t.Fatalf("Get flood %d", gets)
	}
}

func TestRestartVMRefreshesIPAndRematerializes(t *testing.T) {
	rt := fake.New()
	rt.IPs = []string{"192.168.64.2", "192.168.64.9"}
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	pod := samplePod("rs", "uid-rs")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "sec",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "sec", MountPath: "/etc/sec"}}
	creds := Credentials{Secrets: map[string]*corev1.Secret{
		"s": {Data: map[string][]byte{"token": []byte("shh")}},
	}}
	if err := e.Create(context.Background(), pod, creds); err != nil {
		t.Fatal(err)
	}
	got := waitPhase(t, e, "default", "rs", corev1.PodRunning)
	if got.Status.PodIP != "192.168.64.2" {
		t.Fatalf("initial ip %s", got.Status.PodIP)
	}
	before := rt.MaterializeCalls()
	if before < 1 {
		t.Fatal("expected initial materialize")
	}
	e.mu.RLock()
	rec := e.pods["default/rs"]
	e.mu.RUnlock()
	if rec == nil {
		t.Fatal("missing record")
	}
	if err := e.restartVM(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	got, err := e.Get("default", "rs")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.PodIP != "192.168.64.9" {
		t.Fatalf("ip after restart %s", got.Status.PodIP)
	}
	if rt.MaterializeCalls() <= before {
		t.Fatalf("copy-mode volumes not rematerialized: before=%d after=%d", before, rt.MaterializeCalls())
	}
	_ = e.Delete(context.Background(), "default", "rs", 0)
}

func TestPostStartHook(t *testing.T) {
	e := testEngine(t)
	pod := samplePod("ps", "uid-ps")
	pod.Spec.Containers[0].Lifecycle = &corev1.Lifecycle{
		PostStart: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}},
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "ps")
		if err == nil && p.Status.Phase == corev1.PodRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	p, _ := e.Get("default", "ps")
	t.Fatalf("postStart: %s %s", p.Status.Phase, p.Status.Message)
}

type slowListRuntime struct {
	sidecar.Runtime
	delay time.Duration
}

func (s slowListRuntime) List(ctx context.Context, ns, name string) ([]sidecar.Status, error) {
	time.Sleep(s.delay)
	return s.Runtime.List(ctx, ns, name)
}

func TestGetUsesCachedSidecarStatus(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	sc := slowListRuntime{Runtime: sidecar.NewMemory(), delay: 200 * time.Millisecond}
	e := New(cfg, slots, fake.New(), sc, event.Nop{}, "10.0.0.1")
	pod := samplePod("hy-cache", "uid-hy-cache")
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "log", Image: "busybox"})
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	_ = waitPhase(t, e, "default", "hy-cache", corev1.PodRunning)

	start := time.Now()
	p, err := e.Get("default", "hy-cache")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed >= 50*time.Millisecond {
		t.Fatalf("Get took %v; sidecar List must not run under rec.mu", elapsed)
	}
	if len(p.Status.ContainerStatuses) != 2 {
		t.Fatalf("container statuses %d", len(p.Status.ContainerStatuses))
	}

	start = time.Now()
	_ = e.List()
	if time.Since(start) >= 50*time.Millisecond {
		t.Fatalf("List took %v; expected cached sidecar statuses", time.Since(start))
	}
	_ = e.Delete(context.Background(), "default", "hy-cache", 0)
}

type countingRecorder struct {
	mu     sync.Mutex
	normal []string
}

func (c *countingRecorder) Normal(_ context.Context, reason, message string) {
	c.mu.Lock()
	c.normal = append(c.normal, reason)
	c.mu.Unlock()
}

func (c *countingRecorder) Warn(context.Context, string, string) {}

func (c *countingRecorder) reasons() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.normal))
	copy(out, c.normal)
	return out
}

func TestStartEmitsProgressEvents(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	rec := &countingRecorder{}
	e := New(cfg, slots, fake.New(), sidecar.None{}, rec, "10.0.0.1")
	if err := e.Create(context.Background(), samplePod("ev", "uid-ev"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	_ = waitPhase(t, e, "default", "ev", corev1.PodRunning)
	got := rec.reasons()
	if len(got) < 3 {
		t.Fatalf("want >= 3 progress events, got %v", got)
	}
	want := map[string]bool{event.ReasonCreated: false, event.ReasonStarting: false, event.ReasonVMDialing: false, event.ReasonStarted: false}
	for _, r := range got {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for r, seen := range want {
		if !seen {
			t.Fatalf("missing event %s in %v", r, got)
		}
	}
	_ = e.Delete(context.Background(), "default", "ev", 0)
}

func TestPersistMACRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, err := loadMAC(dir); err == nil {
		t.Fatal("expected missing mac")
	}
	const mac = "0a:1b:2c:3d:4e:5f"
	if err := persistMAC(dir, mac); err != nil {
		t.Fatal(err)
	}
	got, err := loadMAC(dir)
	if err != nil || got != mac {
		t.Fatalf("got %q %v", got, err)
	}
}

func TestPodMACReusedForSameUID(t *testing.T) {
	e := testEngine(t)
	const uid = "uid-mac-reuse"
	a, err := e.podMAC(uid)
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.podMAC(uid)
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatalf("MAC changed across start attempts: %s -> %s", a, b)
	}
	got, err := loadMAC(podMACDir(e.cfg.CacheDir, uid))
	if err != nil || got != a {
		t.Fatalf("file %q %v want %s", got, err, a)
	}
	raw, err := os.ReadFile(filepath.Join(podMACDir(e.cfg.CacheDir, uid), "mac.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		t.Fatal("mac.txt empty")
	}
}

func TestReadinessNotBlockedByHangingLiveness(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	cfg.ProbeInterval = 10 * time.Second
	rt := fake.New()
	rt.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if len(req.Argv) > 0 && req.Argv[0] == "hang" {
			time.Sleep(2 * time.Second)
			return 0, nil
		}
		if len(req.Argv) > 0 && req.Argv[0] == "unready" {
			return 1, nil
		}
		return 0, nil
	}
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	pod := samplePod("hol", "uid-hol")
	pod.Spec.Containers[0].ReadinessProbe = &corev1.Probe{
		ProbeHandler:   corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"unready"}}},
		TimeoutSeconds: 1,
		PeriodSeconds:  1,
	}
	pod.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"hang"}}},
		TimeoutSeconds:   1,
		PeriodSeconds:    1,
		FailureThreshold: 3,
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	_ = waitPhase(t, e, "default", "hol", corev1.PodRunning)
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "hol")
		if err == nil && p.Status.Phase == corev1.PodRunning && !podReady(p) {
			_ = e.Delete(context.Background(), "default", "hol", 0)
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ := e.Get("default", "hol")
	_ = e.Delete(context.Background(), "default", "hol", 0)
	t.Fatalf("readiness should flip false well before liveness hang: ready=%v phase=%s", podReady(p), p.Status.Phase)
}
