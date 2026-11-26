package kubeletconformance

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/engine"
	"github.com/mlvea/darwin-node/pkg/runtime/simvz"

	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kexec "k8s.io/utils/exec"
)

// TestKubeletConformance pins the kubelet pod lifecycle this node already
// implements. Each case names the change it guards. A real miss is a
// failing assertion; a behavior that is wrong on main is t.Skip, not a
// product edit in this package.
func TestKubeletConformance(t *testing.T) {
	cases := []struct {
		name string
		fn   func(*testing.T)
	}{
		// PR #34 (5101433): LastTransitionTime moves only when the condition flips.
		{"ready-transition-time", caseReadyTransition},
		// PR #17 (5ef9033): liveness failure restarts Always, OnFailure, and the empty policy.
		{"liveness-restart-policy", caseLivenessRestarts},
		// PR #17: RestartPolicy Never fails the pod and frees the slot.
		{"liveness-never-fails", caseLivenessNever},
		// PR #17: the CrashLoop delay returns to 10s after 10m healthy. The count does not.
		{"liveness-backoff-resets", caseLivenessBackoffResets},
		// PR #17: a liveness restart clears Started until the startup probe passes again.
		{"liveness-clears-started", caseLivenessClearsStarted},
		// PR #34: a Succeeded Never pod stays Succeeded across a process restart.
		{"node-restart-keeps-succeeded", caseNodeRestartKeepsSucceeded},
		// PR #34: a Running Never pod is Failed/NodeRestarted and is not booted.
		{"node-restart-never-running", caseNodeRestartNeverRunning},
		// PR #34: Always is booted again and the restart count grows by one.
		{"node-restart-always-reboots", caseNodeRestartAlways},
		// PR #34: a Never pod with no previous sandbox still starts.
		{"fresh-never-still-starts", caseFreshNeverStarts},
		// PR #32: a dead guest agent does not fail the pod or restart the VM.
		{"guest-agent-loss-stays-running", caseGuestAgentLoss},
		// PR #56 (63d46f5): the cache store is committed before Succeeded is visible.
		{"cache-committed-before-succeeded", caseCacheCommitted},
		// PR #49 (196bf9c): a non-zero exit does not replace the cache store.
		{"cache-nonzero-does-not-replace", caseCacheNonZero},
		// PR #23 (30169cb): an HTTP probe named port is the container port.
		{"named-port-resolves", caseNamedPort},
		// PR #23: an unknown named port does not become ready.
		{"unknown-named-port", caseUnknownNamedPort},
		// Init containers run in order, and a failure never boots the VM.
		{"init-order", caseInitOrder},
		{"init-failure-skips-vm", caseInitFailure},
		// PR #27 (080b5e9): workload exit follows restartPolicy.
		{"workload-exit-policy", caseWorkloadExit},
		// PR #27: phase walks Pending, Running, then a terminal phase, and stays there.
		{"phase-monotonic", casePhaseMonotonic},
		// PR #48 (d9dd406) and PR #35: exec publishes the process status; attach is refused.
		{"exec-exit-and-attach", caseExecExit},
		// PR #32: a stalled exec reader keeps every stdout byte.
		{"exec-backpressure", caseExecBackpressure},
		// PR #36 (bf6ed9a): follow returns a line that was already buffered.
		{"log-follow-snapshot", caseLogSnapshot},
		// PR #33 (f8ffb20): image pull backs off in Pending and does not hold a slot.
		{"image-pull-backoff", caseImagePullBackoff},
		// PR #34: a failed boot backs off, releases the slot, then reaches Running.
		{"boot-backoff", caseBootBackoff},
		// PR #16 (7a0ae3f) and PR #17: sidecar CrashLoop holds the slot; Never frees it.
		{"sidecar-crashloop", caseSidecarCrashLoop},
		{"sidecar-never-fails", caseSidecarNever},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.fn)
	}
}

func caseReadyTransition(t *testing.T) {
	flag := filepath.Join(t.TempDir(), "ready")
	if err := os.WriteFile(flag, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, harnessOpt{})
	pod := h.pod("ready", "uid-ready", corev1.RestartPolicyAlways)
	pod.Spec.Containers[0].ReadinessProbe = execProbe("test -f "+strconv.Quote(flag), 3600)
	h.create(pod)
	p := h.wait("default", "ready", "ready", podReady)
	readyAt := condTime(p, corev1.PodReady)
	schedAt := condTime(p, corev1.PodScheduled)
	if readyAt.IsZero() || schedAt.IsZero() {
		t.Fatalf("missing transition times: %+v", p.Status.Conditions)
	}
	again, err := h.get("default", "ready")
	if err != nil {
		t.Fatal(err)
	}
	againReady, againSched := condTime(again, corev1.PodReady), condTime(again, corev1.PodScheduled)
	if !againReady.Equal(&readyAt) || !againSched.Equal(&schedAt) {
		t.Fatal("re-reading status rewrote LastTransitionTime")
	}
	// A minute of virtual time is not a probe period, so a correct stamp stays put.
	h.clk.Advance(time.Minute)
	held, err := h.get("default", "ready")
	if err != nil {
		t.Fatal(err)
	}
	heldReady, heldSched := condTime(held, corev1.PodReady), condTime(held, corev1.PodScheduled)
	if !heldReady.Equal(&readyAt) || !heldSched.Equal(&schedAt) {
		t.Fatalf("transition time moved without a status flip: ready %s -> %s", readyAt, heldReady)
	}
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	h.waitArmed()
	h.clk.Advance(time.Hour)
	flipped := h.wait("default", "ready", "not ready", func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodRunning && !podReady(p)
	})
	flipReady, flipSched := condTime(flipped, corev1.PodReady), condTime(flipped, corev1.PodScheduled)
	if flipReady.Equal(&readyAt) {
		t.Fatal("PodReady LastTransitionTime stayed put across the flip to False")
	}
	if !flipSched.Equal(&schedAt) {
		t.Fatal("PodScheduled LastTransitionTime moved when only PodReady flipped")
	}
}

func caseLivenessRestarts(t *testing.T) {
	policies := []struct {
		name   string
		policy corev1.RestartPolicy
	}{
		{"always", corev1.RestartPolicyAlways},
		{"on-failure", corev1.RestartPolicyOnFailure},
		{"empty", ""},
	}
	for _, tc := range policies {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOpt{})
			pod := h.pod("lv-"+tc.name, "uid-lv-"+tc.name, tc.policy)
			pod.Spec.Containers[0].LivenessProbe = execProbe("exit 1", 3600)
			h.create(pod)
			h.wait("default", pod.Name, "crash loop", func(p *corev1.Pod) bool {
				return p.Status.Phase == corev1.PodRunning && p.Status.Reason == "CrashLoopBackOff"
			})
			if h.starts() != 1 {
				t.Fatalf("starts=%d before the 10s delay", h.starts())
			}
			h.waitArmed()
			h.clk.Advance(10*time.Second - time.Millisecond)
			h.holdStarts(1, 25*time.Millisecond)
			h.clk.Advance(time.Millisecond)
			got := h.wait("default", pod.Name, "restarted", func(p *corev1.Pod) bool {
				return p.Status.Phase == corev1.PodRunning && restartCount(p) == 1 && h.starts() == 2
			})
			if got.Status.Phase == corev1.PodFailed {
				t.Fatalf("liveness failed the pod: %s", statusLine(got))
			}
			if h.e.Slots().Used() != 1 || h.rt.Running() != 1 {
				t.Fatalf("slots=%d running=%d, CrashLoop released the VM", h.e.Slots().Used(), h.rt.Running())
			}
		})
	}
}

func caseLivenessNever(t *testing.T) {
	h := newHarness(t, harnessOpt{})
	pod := h.pod("lv-never", "uid-lv-never", corev1.RestartPolicyNever)
	pod.Spec.Containers[0].LivenessProbe = execProbe("exit 1", 3600)
	h.create(pod)
	p := h.waitPhase("default", "lv-never", corev1.PodFailed)
	if h.starts() != 1 {
		t.Fatalf("starts=%d, Never restarted the VM", h.starts())
	}
	if h.e.Slots().Used() != 0 || h.rt.Running() != 0 {
		t.Fatalf("slots=%d running=%d, a failed Never pod kept its VM", h.e.Slots().Used(), h.rt.Running())
	}
	if p.Status.Reason == "CrashLoopBackOff" {
		t.Fatalf("Never stayed in CrashLoopBackOff: %s", statusLine(p))
	}
}

func caseLivenessBackoffResets(t *testing.T) {
	var samples atomic.Int32
	engine.SetProbeObserver(func(kind string, _ time.Duration, _ error) {
		if kind == "liveness" {
			samples.Add(1)
		}
	})
	t.Cleanup(func() { engine.SetProbeObserver(nil) })

	flag := filepath.Join(t.TempDir(), "live")
	h := newHarness(t, harnessOpt{})
	pod := h.pod("reset", "uid-reset", corev1.RestartPolicyAlways)
	pod.Spec.Containers[0].LivenessProbe = execProbe("test -f "+strconv.Quote(flag), 3600)
	h.create(pod)
	h.wait("default", "reset", "first backoff", func(p *corev1.Pod) bool {
		return p.Status.Reason == "CrashLoopBackOff" && h.starts() == 1
	})
	h.waitArmed()
	h.clk.Advance(10*time.Second - time.Millisecond)
	h.holdStarts(1, 25*time.Millisecond)
	h.clk.Advance(time.Millisecond)
	h.wait("default", "reset", "first restart", func(p *corev1.Pod) bool {
		return h.starts() == 2 && restartCount(p) == 1 && p.Status.Phase == corev1.PodRunning
	})

	if err := os.WriteFile(flag, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.waitArmed()
	seen := samples.Load()
	h.clk.Advance(time.Millisecond)
	waitSamples(t, &samples, seen)

	// Ten minutes without a restart is the kubelet healthy window. The
	// probe period is an hour, so this advance must not sample liveness.
	h.waitArmed()
	seen = samples.Load()
	h.clk.Advance(10 * time.Minute)
	h.holdStarts(2, 20*time.Millisecond)
	if samples.Load() != seen {
		t.Fatal("liveness ran during the healthy window")
	}
	if err := os.Remove(flag); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(time.Hour)
	waitSamples(t, &samples, seen)
	h.wait("default", "reset", "backoff after the window", func(p *corev1.Pod) bool {
		return p.Status.Reason == "CrashLoopBackOff" && h.starts() == 2
	})
	h.waitArmed()
	// A delay that was still 20s would not fire. The window puts it back at 10s.
	h.clk.Advance(10*time.Second - time.Millisecond)
	h.holdStarts(2, 25*time.Millisecond)
	h.clk.Advance(time.Millisecond)
	got := h.wait("default", "reset", "delay reset to 10s", func(p *corev1.Pod) bool {
		return h.starts() == 3 && restartCount(p) == 2 && p.Status.Phase == corev1.PodRunning
	})
	if restartCount(got) == 0 {
		t.Fatal("the healthy window cleared the restart count")
	}
}

func caseLivenessClearsStarted(t *testing.T) {
	h := newHarness(t, harnessOpt{})
	pod := h.pod("started", "uid-started", corev1.RestartPolicyAlways)
	pod.Spec.Containers[0].StartupProbe = execProbe("exit 0", 3600)
	pod.Spec.Containers[0].LivenessProbe = execProbe("exit 1", 3600)
	h.create(pod)
	h.wait("default", "started", "crash loop", func(p *corev1.Pod) bool {
		return p.Status.Reason == "CrashLoopBackOff" && containerStarted(p)
	})
	h.waitArmed()
	h.clk.Advance(10 * time.Second)
	// The next startup sample is a virtual millisecond away. Leaving the
	// clock there keeps Started false, which is the bit a restart must clear.
	h.wait("default", "started", "started cleared", func(p *corev1.Pod) bool {
		return restartCount(p) == 1 && p.Status.Phase == corev1.PodRunning && !containerStarted(p)
	})
}

func caseNodeRestartKeepsSucceeded(t *testing.T) {
	h := newHarness(t, harnessOpt{interrupted: []string{"uid-kept"}})
	pod := h.pod("kept", "uid-kept", corev1.RestartPolicyNever)
	pod.Status = corev1.PodStatus{
		Phase:  corev1.PodSucceeded,
		Reason: "Completed",
		ContainerStatuses: []corev1.ContainerStatus{{
			Name: "macos",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0, Reason: "Completed",
			}},
		}},
	}
	h.create(pod)
	p, err := h.get("default", "kept")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("phase %s, a finished Never pod was restarted with the node", p.Status.Phase)
	}
	term := terminatedState(p)
	if term == nil || term.ExitCode != 0 {
		t.Fatalf("exit lost across the node restart: %+v", term)
	}
	if h.starts() != 0 || h.e.Slots().Used() != 0 {
		t.Fatalf("starts=%d slots=%d, Succeeded booted a VM", h.starts(), h.e.Slots().Used())
	}
}

func caseNodeRestartNeverRunning(t *testing.T) {
	h := newHarness(t, harnessOpt{interrupted: []string{"uid-cut"}})
	pod := h.pod("cut", "uid-cut", corev1.RestartPolicyNever)
	pod.Status = runningStatus(4)
	h.create(pod)
	p, err := h.get("default", "cut")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase != corev1.PodFailed || p.Status.Reason != "NodeRestarted" {
		t.Fatalf("status %s, a Running Never pod was booted or left Running", statusLine(p))
	}
	if restartCount(p) != 4 {
		t.Fatalf("restart count %d, NodeRestarted changed it", restartCount(p))
	}
	if h.starts() != 0 || h.e.Slots().Used() != 0 {
		t.Fatalf("starts=%d slots=%d", h.starts(), h.e.Slots().Used())
	}
}

func caseNodeRestartAlways(t *testing.T) {
	h := newHarness(t, harnessOpt{interrupted: []string{"uid-again"}})
	pod := h.pod("again", "uid-again", corev1.RestartPolicyAlways)
	pod.Status = runningStatus(2)
	h.create(pod)
	p := h.wait("default", "again", "rebooted", func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodRunning && restartCount(p) == 3 && h.starts() == 1
	})
	if p.Status.Phase == corev1.PodFailed {
		t.Fatalf("Always was failed on node restart: %s", statusLine(p))
	}
}

func caseFreshNeverStarts(t *testing.T) {
	// A different UID was on disk. This pod was not, so the node restart
	// must not mark it Failed.
	h := newHarness(t, harnessOpt{interrupted: []string{"uid-other"}})
	pod := h.pod("fresh", "uid-fresh", corev1.RestartPolicyNever)
	h.create(pod)
	p := h.waitPhase("default", "fresh", corev1.PodRunning)
	if p.Status.Reason == "NodeRestarted" || h.starts() != 1 {
		t.Fatalf("fresh Never pod did not start: %s starts=%d", statusLine(p), h.starts())
	}
}

func caseGuestAgentLoss(t *testing.T) {
	restore := engine.SetAgentRedialPaceForTest(time.Millisecond, 5*time.Millisecond, 30*time.Millisecond)
	t.Cleanup(restore)
	h := newHarness(t, harnessOpt{agentTimeout: 400 * time.Millisecond})
	pod := h.pod("agent", "uid-agent", corev1.RestartPolicyNever)
	h.create(pod)
	h.wait("default", "agent", "ready", func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodRunning && podReady(p)
	})
	if err := h.rt.CrashGuest("default", "agent"); err != nil {
		t.Fatal(err)
	}
	p := h.wait("default", "agent", "agent lost", func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodRunning && !podReady(p) && p.Status.Reason == "GuestAgentDisconnected"
	})
	if restartCount(p) != 0 || h.starts() != 1 {
		t.Fatalf("guest loss restarted the pod: %s starts=%d", statusLine(p), h.starts())
	}
	if h.e.Slots().Used() != 1 || h.rt.Running() != 1 {
		t.Fatalf("slots=%d running=%d, a disconnected agent dropped the VM", h.e.Slots().Used(), h.rt.Running())
	}
	if !containerStarted(p) {
		t.Fatal("Started was cleared while the VM was still the same container")
	}
}

func caseCacheCommitted(t *testing.T) {
	guest := filepath.Join(t.TempDir(), "dd")
	flag := filepath.Join(t.TempDir(), "go")
	h := newHarness(t, harnessOpt{writers: []string{"default"}})
	seedCache(t, h.cache, "old\n")
	pod := cachePod(h, "saved", "uid-saved", guest, corev1.RestartPolicyNever)
	// SimVZ does not mount virtio-fs, so a write to the guest path does not
	// land in the share. The bytes are written on the host side of that
	// share, which is where a Darwin guest write shows up, and the workload
	// then exits 0.
	pod.Spec.Containers[0].Command = []string{"/bin/sh", "-c", "while [ ! -f " + strconv.Quote(flag) + " ]; do sleep 0.02; done; exit 0"}
	h.create(pod)
	h.waitPhase("default", "saved", corev1.PodRunning)
	writeShare(t, h.cache, "uid-saved", "built\n")
	if err := os.WriteFile(flag, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := h.waitExit("default", "saved"); code != 0 {
		t.Fatalf("exit %d", code)
	}
	running, err := h.get("default", "saved")
	if err != nil {
		t.Fatal(err)
	}
	if running.Status.Phase == corev1.PodSucceeded {
		t.Fatal("Succeeded was published before the VM stop that carries the exit")
	}
	h.stopVM()
	h.nudgeWatch()
	h.waitPhase("default", "saved", corev1.PodSucceeded)
	// The same observation that shows Succeeded must already show the store.
	// A commit that happens after the phase flip loses that race.
	if got := readStore(t, h.cache); got != "built\n" {
		t.Fatalf("store %q at Succeeded, the commit was not visible with the phase", got)
	}
}

func caseCacheNonZero(t *testing.T) {
	guest := filepath.Join(t.TempDir(), "dd")
	flag := filepath.Join(t.TempDir(), "go")
	h := newHarness(t, harnessOpt{writers: []string{"default"}})
	seedCache(t, h.cache, "old\n")
	pod := cachePod(h, "kept", "uid-cache-kept", guest, corev1.RestartPolicyNever)
	pod.Spec.Containers[0].Command = []string{"/bin/sh", "-c", "while [ ! -f " + strconv.Quote(flag) + " ]; do sleep 0.02; done; exit 1"}
	h.create(pod)
	h.waitPhase("default", "kept", corev1.PodRunning)
	writeShare(t, h.cache, "uid-cache-kept", "poison\n")
	if err := os.WriteFile(flag, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := h.waitExit("default", "kept"); code != 1 {
		t.Fatalf("exit %d", code)
	}
	h.stopVM()
	h.nudgeWatch()
	h.waitPhase("default", "kept", corev1.PodFailed)
	if got := readStore(t, h.cache); got != "old\n" {
		t.Fatalf("store %q, a failed run replaced the cache", got)
	}
}

func caseNamedPort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	port := mustPort(t, srv.URL)
	h := newHarness(t, harnessOpt{})
	pod := h.pod("named", "uid-named", corev1.RestartPolicyAlways)
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: port}}
	pod.Spec.Containers[0].ReadinessProbe = httpProbe("http")
	h.create(pod)
	h.wait("default", "named", "ready via named port", podReady)
}

func caseUnknownNamedPort(t *testing.T) {
	h := newHarness(t, harnessOpt{})
	pod := h.pod("unknown", "uid-unknown", corev1.RestartPolicyAlways)
	pod.Spec.Containers[0].Ports = []corev1.ContainerPort{{Name: "http", ContainerPort: 9}}
	pod.Spec.Containers[0].ReadinessProbe = httpProbe("missing")
	h.create(pod)
	p := h.wait("default", "unknown", "unknown port", func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodRunning && !podReady(p) && p.Status.Reason == "ReadinessProbe"
	})
	if podReady(p) {
		t.Fatal("an unknown named port became ready")
	}
}

func caseInitOrder(t *testing.T) {
	sc := newScript(map[string]int{"setup": 0, "migrate": 0})
	h := newHarness(t, harnessOpt{sc: sc})
	pod := h.pod("inits", "uid-inits", corev1.RestartPolicyNever)
	pod.Spec.InitContainers = []corev1.Container{
		{Name: "setup", Image: "example.invalid/init:1"},
		{Name: "migrate", Image: "example.invalid/init:1"},
	}
	h.create(pod)
	h.waitPhase("default", "inits", corev1.PodRunning)
	if got := strings.Join(sc.Order(), ","); got != "setup,migrate" {
		t.Fatalf("init order %s", got)
	}
	if h.starts() != 1 {
		t.Fatalf("starts=%d", h.starts())
	}
}

func caseInitFailure(t *testing.T) {
	sc := newScript(map[string]int{"setup": 0, "migrate": 1})
	h := newHarness(t, harnessOpt{sc: sc})
	pod := h.pod("init-fail", "uid-init-fail", corev1.RestartPolicyAlways)
	pod.Spec.InitContainers = []corev1.Container{
		{Name: "setup", Image: "example.invalid/init:1"},
		{Name: "migrate", Image: "example.invalid/init:1"},
	}
	h.create(pod)
	p := h.waitPhase("default", "init-fail", corev1.PodFailed)
	if got := strings.Join(sc.Order(), ","); got != "setup,migrate" {
		t.Fatalf("init order %s", got)
	}
	if h.starts() != 0 || h.rt.Running() != 0 || h.e.Slots().Used() != 0 {
		t.Fatalf("starts=%d running=%d slots=%d, a failed init booted a VM", h.starts(), h.rt.Running(), h.e.Slots().Used())
	}
	for _, ph := range h.phases("default", "init-fail") {
		if ph == corev1.PodRunning {
			t.Fatal("init failure passed through Running")
		}
	}
	if !strings.Contains(p.Status.Message, "exited") {
		t.Fatalf("message %q", p.Status.Message)
	}
}

func caseWorkloadExit(t *testing.T) {
	cases := []struct {
		name    string
		policy  corev1.RestartPolicy
		code    int
		restart bool
		succeed bool
	}{
		{"never-zero", corev1.RestartPolicyNever, 0, false, true},
		{"on-failure-zero", corev1.RestartPolicyOnFailure, 0, false, true},
		{"always-zero", corev1.RestartPolicyAlways, 0, true, false},
		{"empty-zero", "", 0, true, false},
		{"never-nonzero", corev1.RestartPolicyNever, 7, false, false},
		{"on-failure-nonzero", corev1.RestartPolicyOnFailure, 7, true, false},
		{"always-nonzero", corev1.RestartPolicyAlways, 7, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOpt{})
			pod := h.pod("job", "uid-job-"+tc.name, tc.policy)
			pod.Spec.Containers[0].Command = []string{"/bin/sh", "-c", fmt.Sprintf("exit %d", tc.code)}
			h.create(pod)
			h.waitPhase("default", "job", corev1.PodRunning)
			before := h.starts()
			if got := h.waitExit("default", "job"); int(got) != tc.code {
				t.Fatalf("exit %d, want %d", got, tc.code)
			}
			h.stopVM()
			h.nudgeWatch()
			if tc.restart {
				p := h.wait("default", "job", "restarted", func(p *corev1.Pod) bool {
					return p.Status.Phase == corev1.PodRunning && h.starts() == before+1 && restartCount(p) == 1
				})
				h.holdStarts(before+1, 30*time.Millisecond)
				if h.e.Slots().Used() != 1 {
					t.Fatalf("slots=%d after a workload restart", h.e.Slots().Used())
				}
				if p.Status.Phase != corev1.PodRunning {
					t.Fatalf("phase %s", p.Status.Phase)
				}
				return
			}
			want := corev1.PodFailed
			if tc.succeed {
				want = corev1.PodSucceeded
			}
			p := h.waitPhase("default", "job", want)
			term := terminatedState(p)
			if term == nil || int(term.ExitCode) != tc.code {
				t.Fatalf("terminated %+v, want exit %d", term, tc.code)
			}
			if tc.succeed && term.Reason != "Completed" {
				t.Fatalf("reason %q", term.Reason)
			}
			if !tc.succeed && term.Reason != "Error" {
				t.Fatalf("reason %q", term.Reason)
			}
			if h.starts() != before {
				t.Fatalf("starts %d -> %d", before, h.starts())
			}
			if h.e.Slots().Used() != 0 {
				t.Fatalf("slots=%d, a finished workload kept the VM", h.e.Slots().Used())
			}
		})
	}
}

func casePhaseMonotonic(t *testing.T) {
	h := newHarness(t, harnessOpt{})
	pod := h.pod("phase", "uid-phase", corev1.RestartPolicyNever)
	pod.Spec.Containers[0].Command = []string{"/bin/sh", "-c", "exit 0"}
	h.create(pod)
	h.waitPhase("default", "phase", corev1.PodRunning)
	if h.waitExit("default", "phase") != 0 {
		t.Fatal("exit")
	}
	h.stopVM()
	h.nudgeWatch()
	h.waitPhase("default", "phase", corev1.PodSucceeded)
	got := h.phases("default", "phase")
	want := []corev1.PodPhase{corev1.PodPending, corev1.PodRunning, corev1.PodSucceeded}
	if strings.Join(phaseNames(got), ",") != strings.Join(phaseNames(want), ",") {
		t.Fatalf("phases %v", got)
	}
	again, err := h.get("default", "phase")
	if err != nil {
		t.Fatal(err)
	}
	if again.Status.Phase != corev1.PodSucceeded {
		t.Fatalf("phase left Succeeded for %s", again.Status.Phase)
	}
}

func caseExecExit(t *testing.T) {
	h := newHarness(t, harnessOpt{})
	pod := h.pod("exec", "uid-exec", corev1.RestartPolicyAlways)
	h.create(pod)
	h.wait("default", "exec", "running", func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodRunning && podReady(p)
	})
	ctx, cancel := context.WithTimeout(context.Background(), wallBudget)
	defer cancel()
	err := h.p.RunInContainer(ctx, "default", "exec", "macos", []string{"/bin/sh", "-c", "exit 7"}, discardAttach{})
	var exitErr kexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitStatus() != 7 {
		t.Fatalf("exec exit: %v", err)
	}
	if err := h.p.RunInContainer(ctx, "default", "exec", "macos", []string{"/bin/sh", "-c", "exit 0"}, discardAttach{}); err != nil {
		t.Fatalf("exit 0: %v", err)
	}
	err = h.p.AttachToContainer(ctx, "default", "exec", "macos", discardAttach{})
	if err == nil || !strings.Contains(err.Error(), "attach is not supported") {
		t.Fatalf("attach: %v", err)
	}
}

func caseExecBackpressure(t *testing.T) {
	h := newHarness(t, harnessOpt{})
	pod := h.pod("slow", "uid-slow", corev1.RestartPolicyAlways)
	h.create(pod)
	h.wait("default", "slow", "running", func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodRunning && podReady(p)
	})
	out := &pauseWriter{resume: make(chan struct{})}
	errCh := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), wallBudget)
	defer cancel()
	go func() {
		errCh <- h.p.RunInContainer(ctx, "default", "slow", "macos",
			[]string{"/bin/dd", "if=/dev/zero", "bs=4096", "count=64"},
			stdoutAttach{out})
	}()
	deadline := time.Now().Add(3 * time.Second)
	for !out.paused.Load() {
		if time.Now().After(deadline) {
			t.Fatal("stdout reader was not paused")
		}
		time.Sleep(time.Millisecond)
	}
	close(out.resume)
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("exec: %v", err)
		}
	case <-time.After(wallBudget):
		t.Fatal("exec did not finish after the reader resumed")
	}
	if out.Len() != 64*4096 {
		t.Fatalf("kept %d bytes, a stalled reader dropped stdout", out.Len())
	}
}

func caseLogSnapshot(t *testing.T) {
	h := newHarness(t, harnessOpt{})
	pod := h.pod("logs", "uid-logs", corev1.RestartPolicyAlways)
	pod.Spec.Containers[0].Command = []string{"/bin/sh", "-c", "echo conformance-log-line"}
	h.create(pod)
	h.waitPhase("default", "logs", corev1.PodRunning)
	ctx, cancel := context.WithTimeout(context.Background(), wallBudget)
	defer cancel()
	var snap []byte
	deadline := time.Now().Add(wallBudget)
	for time.Now().Before(deadline) {
		rc, err := h.p.GetContainerLogs(ctx, "default", "logs", "macos", api.ContainerLogOpts{})
		if err == nil {
			b, rerr := io.ReadAll(rc)
			_ = rc.Close()
			if rerr == nil && bytes.Contains(b, []byte("conformance-log-line")) {
				snap = b
				break
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	if !bytes.Contains(snap, []byte("conformance-log-line")) {
		t.Fatalf("snapshot logs %q", snap)
	}
	fctx, fcancel := context.WithCancel(ctx)
	defer fcancel()
	follow, err := h.p.GetContainerLogs(fctx, "default", "logs", "macos", api.ContainerLogOpts{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	defer follow.Close()
	buf := readUntil(t, follow, []byte("conformance-log-line"))
	if !bytes.Contains(buf, []byte("conformance-log-line")) {
		t.Fatalf("follow missed the buffered line: %q", buf)
	}
}

func caseImagePullBackoff(t *testing.T) {
	h := newHarness(t, harnessOpt{asVZ: true, pullTimeout: 80 * time.Millisecond})
	pod := h.pod("pull", "uid-pull", corev1.RestartPolicyAlways)
	pod.Spec.Containers[0].Image = "127.0.0.1:1/macos:test"
	h.create(pod)
	p := h.wait("default", "pull", "pull backoff", func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodPending && strings.Contains(p.Status.Message, "back-off") && h.e.Slots().Used() == 0
	})
	if p.Status.Phase == corev1.PodFailed {
		t.Fatalf("pull was permanent: %s", statusLine(p))
	}
	h.waitArmed()
	sib := h.pod("sib", "uid-sib", corev1.RestartPolicyAlways)
	sib.Spec.Containers[0].Image = writeBakedImage(t)
	h.create(sib)
	h.waitPhase("default", "sib", corev1.PodRunning)
	still, err := h.get("default", "pull")
	if err != nil {
		t.Fatal(err)
	}
	if still.Status.Phase != corev1.PodPending || h.e.Slots().Used() != 1 || h.starts() != 1 {
		t.Fatalf("during pull backoff: pull %s slots=%d starts=%d", statusLine(still), h.e.Slots().Used(), h.starts())
	}
}

func caseBootBackoff(t *testing.T) {
	h := newHarness(t, harnessOpt{faults: simvz.Faults{FailStart: 1}})
	pod := h.pod("boot", "uid-boot", corev1.RestartPolicyAlways)
	h.create(pod)
	h.wait("default", "boot", "boot backoff", func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodPending &&
			strings.Contains(p.Status.Message, "back-off") &&
			strings.Contains(p.Status.Message, "VM boot") &&
			h.e.Slots().Used() == 0 &&
			h.starts() == 0
	})
	h.waitArmed()
	// The first delay is 1s before jitter, and jitter stays inside that second.
	h.clk.Advance(time.Second)
	h.wait("default", "boot", "booted", func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodRunning && h.starts() == 1 && h.e.Slots().Used() == 1
	})
}

func caseSidecarCrashLoop(t *testing.T) {
	sc := newScript(map[string]int{"helper": 1})
	h := newHarness(t, harnessOpt{sc: sc})
	pod := h.pod("side", "uid-side", corev1.RestartPolicyAlways)
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "helper", Image: "example.invalid/side:1"})
	h.create(pod)
	h.wait("default", "side", "first restart", func(p *corev1.Pod) bool {
		return p.Status.Phase == corev1.PodRunning && sc.Creates("helper") == 2
	})
	h.waitArmed()
	// The sidecar backoff is wall-clock client-go. An hour of virtual time
	// wakes the watch while that 10s window is still open, so it must not
	// create the container again.
	h.clk.Advance(time.Hour)
	p := h.wait("default", "side", "holding", func(p *corev1.Pod) bool {
		cs := findContainer(p, "helper")
		return cs != nil && cs.State.Waiting != nil && cs.State.Waiting.Reason == "CrashLoopBackOff"
	})
	if sc.Creates("helper") != 2 {
		t.Fatalf("creates=%d, the sidecar restarted inside its backoff", sc.Creates("helper"))
	}
	if p.Status.Phase != corev1.PodRunning || h.e.Slots().Used() != 1 || h.rt.Running() != 1 {
		t.Fatalf("phase %s slots=%d running=%d", p.Status.Phase, h.e.Slots().Used(), h.rt.Running())
	}
}

func caseSidecarNever(t *testing.T) {
	sc := newScript(map[string]int{"helper": 1})
	h := newHarness(t, harnessOpt{sc: sc})
	pod := h.pod("side-never", "uid-side-never", corev1.RestartPolicyNever)
	pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "helper", Image: "example.invalid/side:1"})
	h.create(pod)
	p := h.waitPhase("default", "side-never", corev1.PodFailed)
	if sc.Creates("helper") != 1 {
		t.Fatalf("creates=%d, Never restarted the sidecar", sc.Creates("helper"))
	}
	if h.e.Slots().Used() != 0 || h.rt.Running() != 0 {
		t.Fatalf("slots=%d running=%d", h.e.Slots().Used(), h.rt.Running())
	}
	if !strings.Contains(p.Status.Message, "exited") {
		t.Fatalf("message %q", p.Status.Message)
	}
}

func execProbe(script string, period int32) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", script}},
		},
		PeriodSeconds:    period,
		TimeoutSeconds:   2,
		FailureThreshold: 1,
		SuccessThreshold: 1,
	}
}

func httpProbe(portName string) *corev1.Probe {
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{
				Port: intstr.FromString(portName),
				Path: "/",
			},
		},
		PeriodSeconds:    3600,
		TimeoutSeconds:   2,
		FailureThreshold: 1,
		SuccessThreshold: 1,
	}
}

func runningStatus(count int32) corev1.PodStatus {
	return corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:         "macos",
			RestartCount: count,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{
				StartedAt: metav1.Now(),
			}},
		}},
	}
}

func cachePod(h *harness, name, uid, guest string, policy corev1.RestartPolicy) *corev1.Pod {
	pod := h.pod(name, uid, policy)
	pod.Annotations = map[string]string{
		"cache.darwin.node/xc":       guest,
		"cache-write.darwin.node/xc": "true",
	}
	return pod
}

func seedCache(t *testing.T, cache, body string) {
	t.Helper()
	dir := filepath.Join(cache, "cache-store", "default", "xc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.bin"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeShare writes the host side of cache "xc". On Darwin that directory
// is the virtio-fs share; SimVZ does not mount it into the guest.
func writeShare(t *testing.T, cache, uid, body string) {
	t.Helper()
	dir := filepath.Join(cache, "pods", uid, "caches", "xc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.bin"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readStore(t *testing.T, cache string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(cache, "cache-store", "default", "xc", "state.bin"))
	if err != nil {
		t.Fatalf("cache store: %v", err)
	}
	return string(b)
}

func writeBakedImage(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cfg := `{
		"os":"darwin",
		"hardwareModelData":"abc",
		"machineIdData":"def",
		"storage":[
			{"mediatype":"application/vnd.darwin-node.disk.v1","file":"disk.img"},
			{"mediatype":"application/vnd.darwin-node.aux.v1","file":"aux.img"}
		]
	}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte("DISK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux.img"), []byte("AUX"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustPort(t *testing.T, raw string) int32 {
	t.Helper()
	i := strings.LastIndex(raw, ":")
	if i < 0 {
		t.Fatalf("url %s", raw)
	}
	n, err := strconv.Atoi(raw[i+1:])
	if err != nil {
		t.Fatal(err)
	}
	return int32(n)
}

func waitSamples(t *testing.T, n *atomic.Int32, prev int32) {
	t.Helper()
	deadline := time.Now().Add(wallBudget)
	for time.Now().Before(deadline) {
		if n.Load() > prev {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("probe samples stayed at %d", prev)
}

func phaseNames(in []corev1.PodPhase) []string {
	out := make([]string, len(in))
	for i, p := range in {
		out[i] = string(p)
	}
	return out
}

func readUntil(t *testing.T, r io.Reader, needle []byte) []byte {
	t.Helper()
	ch := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		tmp := make([]byte, 256)
		for !bytes.Contains(buf.Bytes(), needle) {
			n, err := r.Read(tmp)
			if n > 0 {
				_, _ = buf.Write(tmp[:n])
			}
			if err != nil || bytes.Contains(buf.Bytes(), needle) {
				break
			}
		}
		ch <- append([]byte(nil), buf.Bytes()...)
	}()
	select {
	case b := <-ch:
		return b
	case <-time.After(3 * time.Second):
		t.Fatal("follow produced no matching bytes")
		return nil
	}
}

// pauseWriter accepts the first write, then blocks until resume is closed.
// That is the slow kubectl client: the guest must keep the bytes.
type pauseWriter struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	armed  bool
	paused atomic.Bool
	resume chan struct{}
}

func (w *pauseWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	hold := w.armed
	if !w.armed {
		w.armed = true
	}
	if hold {
		w.paused.Store(true)
		w.mu.Unlock()
		<-w.resume
		w.mu.Lock()
	}
	_, _ = w.buf.Write(p)
	w.mu.Unlock()
	return len(p), nil
}

func (w *pauseWriter) Len() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Len()
}

type discardAttach struct{}

func (discardAttach) Stdin() io.Reader            { return nil }
func (discardAttach) Stdout() io.WriteCloser      { return nopWriteCloser{io.Discard} }
func (discardAttach) Stderr() io.WriteCloser      { return nopWriteCloser{io.Discard} }
func (discardAttach) TTY() bool                   { return false }
func (discardAttach) Resize() <-chan api.TermSize { return nil }

type stdoutAttach struct{ w io.Writer }

func (a stdoutAttach) Stdin() io.Reader            { return nil }
func (a stdoutAttach) Stdout() io.WriteCloser      { return nopWriteCloser{a.w} }
func (a stdoutAttach) Stderr() io.WriteCloser      { return nopWriteCloser{io.Discard} }
func (a stdoutAttach) TTY() bool                   { return false }
func (a stdoutAttach) Resize() <-chan api.TermSize { return nil }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
