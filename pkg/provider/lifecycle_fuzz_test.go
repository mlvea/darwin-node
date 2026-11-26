package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/engine"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/node"
	"github.com/mlvea/darwin-node/pkg/runtime/simvz"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/types"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

const (
	// lifeWatchdog is the per-step deadlock bound. A step that is still
	// running when it fires dumps every goroutine and fails the input.
	lifeWatchdog = 5 * time.Second
	// lifeSettle is how long a step waits for pods to leave Pending.
	lifeSettle = 3 * time.Second
	// fdSlack covers descriptors the race detector keeps after the VMs
	// are gone. Soak uses the same allowance.
	fdSlack = 8
	// fdPerRunning is the open-fd budget of one live SimVZ VM (socketpair,
	// vsock listener, accepted sessions, guest client).
	fdPerRunning = 32
)

// errDeadlock is the 5s watchdog. Shutdown is skipped when a step fails
// this way: Drain would block on the same stuck goroutine.
var errDeadlock = errors.New("deadlock watchdog")

// FuzzNodeLifecycle drives random pod create, delete, guest crash, start
// failure, agent restart, and node drain sequences against the provider
// backed by SimVZ. The seed corpus is testdata/fuzz/FuzzNodeLifecycle.
// Fault-free seeds are also listed in testdata/fuzz/node_lifecycle_hardware.jsonl
// for a later Mac Mini replay. Each step checks the lifecycle invariants.
func FuzzNodeLifecycle(f *testing.F) {
	f.Fuzz(func(t *testing.T, data []byte) {
		if err := runLife(t, data); err != nil {
			t.Fatal(err)
		}
	})
}

func runLife(t *testing.T, data []byte) (err error) {
	t.Helper()
	// recoverAgent sleeps on the wall clock. A few milliseconds keeps
	// Delete inside the deadlock watchdog. The pace is process-global;
	// provider tests do not run in parallel.
	restore := engine.SetAgentRedialPaceForTest(time.Millisecond, 5*time.Millisecond, 30*time.Millisecond)
	defer restore()

	raw := append([]byte(nil), data...)
	fdBase, err := warmedFDCount()
	if err != nil {
		return err
	}
	kids, err := childPIDs()
	if err != nil {
		return err
	}
	h := &lifeHarness{
		cache:      t.TempDir(),
		input:      raw,
		ops:        decodeLife(raw),
		fdBase:     fdBase,
		children:   kids,
		leaks:      goleak.IgnoreCurrent(),
		phases:     map[string]corev1.PodPhase{},
		expectDown: map[string]bool{},
	}
	defer func() {
		if h.skipShutdown || errors.Is(err, errDeadlock) {
			return
		}
		shut := runWatched("shutdown", h.shutdown)
		if shut != nil {
			if err == nil {
				err = shut
			} else {
				err = fmt.Errorf("%w\nshutdown: %v", err, shut)
			}
			return
		}
		if err != nil {
			return
		}
		if leak := waitNoLeak(h.leaks, 2*time.Second); leak != nil {
			err = leak
		}
	}()
	if err = runWatched("boot", h.boot); err != nil {
		return err
	}
	for i, op := range h.ops {
		h.step = i
		h.op = op
		stepErr := runWatched(fmt.Sprintf("step %d %s", i, op.Op), h.runStep)
		if stepErr == nil {
			continue
		}
		if errors.Is(stepErr, errDeadlock) {
			h.skipShutdown = true
		}
		return fmt.Errorf("%w\n%s", stepErr, h.dump())
	}
	return nil
}

// runWatched runs fn on another goroutine so a stuck step can still dump
// stacks. t.Fatal stays on the test goroutine.
func runWatched(label string, fn func() error) error {
	errc := make(chan error, 1)
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				errc <- fmt.Errorf("%s panic: %v\n%s", label, rec, debug.Stack())
			}
		}()
		errc <- fn()
	}()
	timer := time.NewTimer(lifeWatchdog)
	defer timer.Stop()
	select {
	case err := <-errc:
		return err
	case <-timer.C:
		return fmt.Errorf("%s %w (%s)\n%s", label, errDeadlock, lifeWatchdog, allStacks())
	}
}

func allStacks() string {
	buf := make([]byte, 64*1024)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return string(buf[:n])
		}
		buf = make([]byte, len(buf)*2)
	}
}

func waitNoLeak(opt goleak.Option, budget time.Duration) error {
	deadline := time.Now().Add(budget)
	var last error
	for {
		last = goleak.Find(opt)
		if last == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("goroutine leak: %w", last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// lifeHarness is one fuzz input. One goroutine mutates it; the watchdog
// only reads stacks.
type lifeHarness struct {
	cache        string
	input        []byte
	ops          []lifeOp
	step         int
	op           lifeOp
	seq          int
	fdBase       int
	children     map[int]struct{}
	leaks        goleak.Option
	phases       map[string]corev1.PodPhase
	expectDown   map[string]bool
	stashed      bool
	skipShutdown bool

	slots *capacity.Slots
	rt    *simvz.Runtime
	eng   *engine.Engine
	p     *Provider
	clk   *engine.VirtualClock
}

func (h *lifeHarness) boot() error {
	simDir := filepath.Join(h.cache, "simvz")
	if err := os.MkdirAll(simDir, 0o755); err != nil {
		return err
	}
	slots, err := capacity.New(2)
	if err != nil {
		return err
	}
	cfg := config.Default()
	cfg.Runtime = "simvz"
	cfg.CacheDir = h.cache
	cfg.NodeName = "fuzz-mac"
	cfg.AllowNATWorkloads = true
	cfg.AgentReadyTimeout = 250 * time.Millisecond
	cfg.ProbeInterval = time.Hour
	cfg.WarmSlots = 0
	cfg.MaxVMs = 2
	cfg.ImageGC = false
	cfg.SerialConsole = false
	rt := simvz.New(simvz.Options{Dir: simDir, Clock: simvz.NewManualClock(), MaxRunning: 2})
	eng := engine.New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.8")
	clk := engine.NewVirtualClock(time.Unix(0, 0).UTC())
	eng.SetVirtualClockForTest(clk)
	p := New(cfg, eng, node.Inventory{
		Host: node.Host{
			LogicalCPUs: 8,
			MemoryBytes: 16 << 30,
			DiskBytes:   100 << 30,
			Arch:        "arm64",
			InternalIP:  "10.0.0.8",
		},
		Cfg: cfg,
	}, event.Nop{})
	h.slots = slots
	h.rt = rt
	h.eng = eng
	h.p = p
	h.clk = clk
	return h.check()
}

func (h *lifeHarness) shutdown() error {
	if h.eng != nil {
		if err := h.eng.Drain(context.Background()); err != nil {
			return err
		}
		if err := h.eng.Close(); err != nil {
			return err
		}
	}
	return h.checkReleased(false)
}

func (h *lifeHarness) runStep() error {
	if err := h.exec(); err != nil {
		return err
	}
	if err := h.settle(); err != nil {
		return err
	}
	return h.check()
}

func (h *lifeHarness) exec() error {
	switch h.op.Op {
	case lifeCreate:
		return h.create()
	case lifeDelete:
		return h.remove()
	case lifeCrash:
		return h.crash()
	case lifeFail:
		n := h.op.Failures
		if n <= 0 {
			n = 1
		}
		h.rt.FailNextStarts(n)
		return nil
	case lifeRestart:
		return h.restartAgent()
	case lifeDrain:
		return h.eng.Drain(context.Background())
	default:
		return fmt.Errorf("unknown op %q", h.op.Op)
	}
}

func (h *lifeHarness) create() error {
	h.seq++
	grace := int64(0)
	policy := corev1.RestartPolicy(h.op.Policy)
	if policy == "" {
		policy = corev1.RestartPolicyAlways
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      fmt.Sprintf("vm-%d", h.op.Slot),
			UID:       k8stypes.UID(fmt.Sprintf("uid-%d", h.seq)),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 policy,
			TerminationGracePeriodSeconds: &grace,
			Containers: []corev1.Container{{
				Name:  "macos",
				Image: "local/macos:test",
			}},
		},
	}
	err := h.p.CreatePod(context.Background(), pod)
	if err == nil || toleratedCreate(err) {
		return nil
	}
	return err
}

func toleratedCreate(err error) bool {
	if err == nil {
		return true
	}
	return errors.Is(err, capacity.ErrVMCapacityExhausted) || strings.Contains(err.Error(), "node is draining")
}

func (h *lifeHarness) remove() error {
	pod, err := h.p.GetPod(context.Background(), "default", fmt.Sprintf("vm-%d", h.op.Slot))
	if err != nil {
		if errdefs.IsNotFound(err) {
			return nil
		}
		return err
	}
	return h.p.DeletePod(context.Background(), pod)
}

func (h *lifeHarness) crash() error {
	err := h.rt.CrashGuest("default", fmt.Sprintf("vm-%d", h.op.Slot))
	if err == nil || errors.Is(err, simvz.ErrNoRunningGuest) || errors.Is(err, simvz.ErrInvalidState) {
		return nil
	}
	return err
}

// restartAgent replaces the provider process. Pod directories are stashed
// across Close so the new engine's Recover sees the previous process's
// pods, then the API objects are created again.
func (h *lifeHarness) restartAgent() error {
	pods, err := h.p.GetPods(context.Background())
	if err != nil {
		return err
	}
	snap := make([]*corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		cp := pod.DeepCopy()
		snap = append(snap, cp)
		if terminalPhase(cp.Status.Phase) || cp.Spec.RestartPolicy == corev1.RestartPolicyNever {
			h.expectDown[string(cp.UID)] = true
		}
	}
	sort.Slice(snap, func(i, j int) bool {
		if snap[i].Namespace != snap[j].Namespace {
			return snap[i].Namespace < snap[j].Namespace
		}
		if snap[i].Name != snap[j].Name {
			return snap[i].Name < snap[j].Name
		}
		return snap[i].UID < snap[j].UID
	})
	if err := h.stashPods(); err != nil {
		return err
	}
	defer func() {
		if h.stashed {
			_ = h.unstashPods()
		}
	}()
	if err := h.eng.Close(); err != nil {
		return err
	}
	if err := h.checkReleased(true); err != nil {
		return err
	}
	if err := h.unstashPods(); err != nil {
		return err
	}
	h.stashed = false
	if err := h.boot(); err != nil {
		return err
	}
	for _, pod := range snap {
		err := h.p.CreatePod(context.Background(), pod)
		if err != nil && !toleratedCreate(err) {
			return err
		}
	}
	return nil
}

func (h *lifeHarness) stashPods() error {
	src := filepath.Join(h.cache, "pods")
	dst := filepath.Join(h.cache, "pods-crash")
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := os.Rename(src, dst); err != nil {
		return err
	}
	h.stashed = true
	return nil
}

func (h *lifeHarness) unstashPods() error {
	src := filepath.Join(h.cache, "pods-crash")
	dst := filepath.Join(h.cache, "pods")
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if st, err := os.Stat(dst); err == nil && st.IsDir() {
		ents, err := os.ReadDir(dst)
		if err != nil {
			return err
		}
		if len(ents) != 0 {
			return fmt.Errorf("cache/pods is not empty while restoring the crashed process")
		}
		if err := os.Remove(dst); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Rename(src, dst)
}

func (h *lifeHarness) settle() error {
	deadline := time.Now().Add(lifeSettle)
	for {
		pods, err := h.p.GetPods(context.Background())
		if err != nil {
			return err
		}
		pending := false
		for _, pod := range pods {
			if !settledPhase(pod.Status.Phase) {
				pending = true
				break
			}
		}
		if !pending {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("pods still pending\n%s", h.dump())
		}
		// The start backoff is a probe timer. Sleep long enough for the
		// engine to arm it, then move the virtual clock past the delay.
		time.Sleep(2 * time.Millisecond)
		if h.clk != nil {
			h.clk.Advance(time.Minute)
		}
	}
}

func settledPhase(p corev1.PodPhase) bool {
	switch p {
	case corev1.PodRunning, corev1.PodFailed, corev1.PodSucceeded:
		return true
	default:
		return false
	}
}

func terminalPhase(p corev1.PodPhase) bool {
	return p == corev1.PodSucceeded || p == corev1.PodFailed
}

func (h *lifeHarness) check() error {
	if h.p == nil || h.rt == nil || h.slots == nil {
		return fmt.Errorf("harness is not booted")
	}
	pods, err := h.p.GetPods(context.Background())
	if err != nil {
		return err
	}
	running := h.rt.Running()
	if running > 2 {
		return fmt.Errorf("running VMs %d", running)
	}
	if h.slots.Used() > 2 || h.slots.Max() != 2 {
		return fmt.Errorf("slots used %d max %d", h.slots.Used(), h.slots.Max())
	}
	phaseRunning := 0
	for _, pod := range pods {
		if pod.Status.Phase == corev1.PodRunning {
			phaseRunning++
		}
	}
	if phaseRunning > 2 {
		return fmt.Errorf("%d pods in Running", phaseRunning)
	}
	if err := h.checkNode(); err != nil {
		return err
	}
	if err := h.observe(pods); err != nil {
		return err
	}
	if err := h.checkDown(pods); err != nil {
		return err
	}
	if err := h.checkOrphans(pods, false); err != nil {
		return err
	}
	if err := h.checkChildren(); err != nil {
		return err
	}
	quiet := running == 0 && h.slots.Used() == 0
	if h.op.Op == lifeDrain && (!quiet || len(pods) != 0) {
		return fmt.Errorf("drain left running %d slots %d pods %d", running, h.slots.Used(), len(pods))
	}
	if err := h.checkFDs(running, quiet); err != nil {
		return err
	}
	if quiet {
		if err := waitNoLeak(h.leaks, 750*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

// checkReleased runs after Close, before the next process boots. allowStash
// permits cache/pods-crash, which is the previous process's pod directory.
func (h *lifeHarness) checkReleased(allowStash bool) error {
	if h.rt != nil && h.rt.Running() != 0 {
		return fmt.Errorf("running %d after release", h.rt.Running())
	}
	if h.slots != nil && h.slots.Used() != 0 {
		return fmt.Errorf("slots used %d after release", h.slots.Used())
	}
	var pods []*corev1.Pod
	if h.p != nil {
		var err error
		pods, err = h.p.GetPods(context.Background())
		if err != nil {
			return err
		}
	}
	if err := h.checkOrphans(pods, allowStash); err != nil {
		return err
	}
	if err := h.checkChildren(); err != nil {
		return err
	}
	if err := h.checkFDs(0, true); err != nil {
		return err
	}
	return waitNoLeak(h.leaks, time.Second)
}

func (h *lifeHarness) checkNode() error {
	n := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "fuzz-mac"}}
	if err := h.p.ConfigureNode(context.Background(), n); err != nil {
		return err
	}
	slot := n.Status.Capacity[corev1.ResourceName(types.ResourceVMSlot)]
	alloc := n.Status.Allocatable[corev1.ResourceName(types.ResourceVMSlot)]
	pods := n.Status.Capacity[corev1.ResourcePods]
	if slot.Value() != 2 || alloc.Value() != 2 || pods.Value() != 2 {
		return fmt.Errorf("vm-slot capacity %d allocatable %d pods %d", slot.Value(), alloc.Value(), pods.Value())
	}
	return nil
}

func (h *lifeHarness) observe(pods []*corev1.Pod) error {
	for _, pod := range pods {
		uid := string(pod.UID)
		phase := pod.Status.Phase
		prev, ok := h.phases[uid]
		if ok && terminalPhase(prev) && !terminalPhase(phase) {
			return fmt.Errorf("pod %s/%s uid %s left %s for %s", pod.Namespace, pod.Name, uid, prev, phase)
		}
		h.phases[uid] = phase
	}
	return nil
}

func (h *lifeHarness) checkDown(pods []*corev1.Pod) error {
	byUID := make(map[string]*corev1.Pod, len(pods))
	for _, pod := range pods {
		byUID[string(pod.UID)] = pod
	}
	var uids []string
	for uid := range h.expectDown {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	for _, uid := range uids {
		pod := byUID[uid]
		if pod == nil {
			continue
		}
		if pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending {
			return fmt.Errorf("pod %s/%s uid %s is %s after it must stay down", pod.Namespace, pod.Name, uid, pod.Status.Phase)
		}
	}
	return nil
}

func (h *lifeHarness) checkOrphans(pods []*corev1.Pod, allowStash bool) error {
	live := make(map[string]struct{}, len(pods))
	for _, pod := range pods {
		live[string(pod.UID)] = struct{}{}
	}
	names, err := podDirNames(h.cache)
	if err != nil {
		return err
	}
	var orphans []string
	for _, name := range names {
		if _, ok := live[name]; !ok {
			orphans = append(orphans, name)
		}
	}
	if len(orphans) > 0 {
		return fmt.Errorf("orphan pod dirs %v", orphans)
	}
	stash := filepath.Join(h.cache, "pods-crash")
	if _, err := os.Stat(stash); err == nil && !allowStash {
		return fmt.Errorf("leftover %s", stash)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if h.rt != nil {
		liveDirs := map[string]struct{}{}
		for _, dir := range h.rt.LiveCloneDirs() {
			liveDirs[filepath.Clean(dir)] = struct{}{}
		}
		matches, err := filepath.Glob(filepath.Join(h.cache, "simvz", "simvz-vm-*"))
		if err != nil {
			return err
		}
		var leftover []string
		for _, match := range matches {
			if _, ok := liveDirs[filepath.Clean(match)]; !ok {
				leftover = append(leftover, match)
			}
		}
		if len(leftover) > 0 {
			return fmt.Errorf("orphan clone dirs %v", leftover)
		}
	}
	return leftoverSnapshots(h.cache)
}

func podDirNames(cache string) ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(cache, "pods"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, ent := range ents {
		if ent.IsDir() {
			names = append(names, ent.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

func leftoverSnapshots(root string) error {
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		name := d.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".old") {
			hits = append(hits, path)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(hits) > 0 {
		return fmt.Errorf("snapshot leftovers %v", hits)
	}
	return nil
}

func (h *lifeHarness) checkFDs(running int, quiet bool) error {
	limit := h.fdBase + fdSlack
	if !quiet {
		if running < 0 {
			running = 0
		}
		limit += fdPerRunning * running
	}
	deadline := time.Now().Add(time.Second)
	var n int
	var err error
	for {
		n, err = simvz.FDCount()
		if err != nil {
			return err
		}
		if n <= limit {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("fd count %d, baseline %d, limit %d, running %d", n, h.fdBase, limit, running)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func warmedFDCount() (int, error) {
	if _, err := simvz.FDCount(); err != nil {
		return 0, err
	}
	return simvz.FDCount()
}

func (h *lifeHarness) checkChildren() error {
	now, err := childPIDs()
	if err != nil {
		return err
	}
	var extra []int
	for pid := range now {
		if _, ok := h.children[pid]; ok {
			continue
		}
		extra = append(extra, pid)
	}
	if len(extra) == 0 {
		return nil
	}
	sort.Ints(extra)
	parts := make([]string, 0, len(extra))
	for _, pid := range extra {
		parts = append(parts, fmt.Sprintf("%d %s", pid, procComm(pid)))
	}
	return fmt.Errorf("child processes %s", strings.Join(parts, ", "))
}

func childPIDs() (map[int]struct{}, error) {
	switch runtime.GOOS {
	case "linux":
		return linuxChildren()
	case "darwin":
		return darwinChildren()
	default:
		return map[int]struct{}{}, nil
	}
}

func linuxChildren() (map[int]struct{}, error) {
	self := os.Getpid()
	task := fmt.Sprintf("/proc/%d/task", self)
	ents, err := os.ReadDir(task)
	if err != nil {
		return nil, err
	}
	out := map[int]struct{}{}
	saw := false
	for _, ent := range ents {
		b, err := os.ReadFile(filepath.Join(task, ent.Name(), "children"))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		saw = true
		for _, field := range strings.Fields(string(b)) {
			pid, err := strconv.Atoi(field)
			if err != nil || pid <= 0 || !processAlive(pid) {
				continue
			}
			out[pid] = struct{}{}
		}
	}
	if saw {
		return out, nil
	}
	return scanProcChildren(self)
}

func scanProcChildren(ppid int) (map[int]struct{}, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := map[int]struct{}{}
	for _, ent := range ents {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "stat"))
		if err != nil {
			continue
		}
		text := string(b)
		i := strings.LastIndex(text, ")")
		if i < 0 || i+2 >= len(text) {
			continue
		}
		fields := strings.Fields(text[i+2:])
		if len(fields) < 2 {
			continue
		}
		parent, err := strconv.Atoi(fields[1])
		if err != nil || parent != ppid || !processAlive(pid) {
			continue
		}
		out[pid] = struct{}{}
	}
	return out, nil
}

func darwinChildren() (map[int]struct{}, error) {
	out, err := exec.Command("ps", "-ax", "-o", "pid=,ppid=").Output()
	if err != nil {
		return nil, err
	}
	self := os.Getpid()
	found := map[int]struct{}{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil || ppid != self || pid == self || !processAlive(pid) {
			continue
		}
		found[pid] = struct{}{}
	}
	return found, nil
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func procComm(pid int) string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (h *lifeHarness) dump() string {
	var b strings.Builder
	fmt.Fprintf(&b, "step %d op %s\n", h.step, scriptJSON([]lifeOp{h.op}))
	fmt.Fprintf(&b, "script %s\n", scriptJSON(h.ops))
	fmt.Fprintf(&b, "input %x\n", h.input)
	running := 0
	if h.rt != nil {
		running = h.rt.Running()
	}
	used, max := 0, 0
	if h.slots != nil {
		used, max = h.slots.Used(), h.slots.Max()
	}
	n, _ := simvz.FDCount()
	fmt.Fprintf(&b, "running %d slots %d/%d fds %d baseline %d\n", running, used, max, n, h.fdBase)
	if h.p != nil {
		pods, err := h.p.GetPods(context.Background())
		if err != nil {
			fmt.Fprintf(&b, "pods: %v\n", err)
		} else {
			sort.Slice(pods, func(i, j int) bool {
				if pods[i].Name == pods[j].Name {
					return pods[i].UID < pods[j].UID
				}
				return pods[i].Name < pods[j].Name
			})
			for _, pod := range pods {
				fmt.Fprintf(&b, "pod %s/%s uid %s phase %s reason %s message %s\n",
					pod.Namespace, pod.Name, pod.UID, pod.Status.Phase, pod.Status.Reason, pod.Status.Message)
			}
		}
	}
	return b.String()
}
