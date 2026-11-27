package vzcontract

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/engine"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime/simvz"
	"github.com/mlvea/darwin-node/pkg/sidecar"

	"go.uber.org/goleak"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

// TestGuestIdentityUniqueness boots six identities on one node: two pods
// one after another, two pods at once on the two VM slots, and one pod
// whose liveness probe restarts the VM twice. Host entropy and guest
// samples are pairwise distinct across every boot, including the restarts.
func TestGuestIdentityUniqueness(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	before := contractFDBefore(t)
	e, rt := contractEngine(t, simvz.Faults{})
	defer finishContract(t, e, before, leaks)

	startPod(t, e, contractPod("seq-a", "uid-seq-a"))
	waitContractPhase(t, e, "seq-a", corev1.PodRunning, 15*time.Second)
	if !sawMethod(rt.Methods("uid-seq-a"), guest.MethodAdopt) {
		t.Fatalf("seq-a methods %v", rt.Methods("uid-seq-a"))
	}
	deletePod(t, e, "seq-a")
	waitContractSlots(t, e, rt, 0)

	startPod(t, e, contractPod("seq-b", "uid-seq-b"))
	waitContractPhase(t, e, "seq-b", corev1.PodRunning, 15*time.Second)
	deletePod(t, e, "seq-b")
	waitContractSlots(t, e, rt, 0)

	startPod(t, e, contractPod("con-a", "uid-con-a"))
	startPod(t, e, contractPod("con-b", "uid-con-b"))
	waitContractPhase(t, e, "con-a", corev1.PodRunning, 15*time.Second)
	waitContractPhase(t, e, "con-b", corev1.PodRunning, 15*time.Second)
	deletePod(t, e, "con-a")
	deletePod(t, e, "con-b")
	waitContractSlots(t, e, rt, 0)

	re := contractPod("reseed", "uid-reseed")
	re.Spec.RestartPolicy = corev1.RestartPolicyAlways
	re.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{
			Exec: &corev1.ExecAction{Command: []string{"/bin/false"}},
		},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 1,
		SuccessThreshold: 1,
	}
	startPod(t, e, re)
	waitContractPhase(t, e, "reseed", corev1.PodRunning, 15*time.Second)
	// The first two restarts wait 10s and then 20s. The third Adopt is the
	// second restart. Delete before the next 40s backoff.
	waitAdoptCount(t, rt, "uid-reseed", 3, 75*time.Second)
	p, err := e.Get("default", "reseed")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status.Phase == corev1.PodFailed {
		t.Fatalf("restart failed the pod: %s", p.Status.Message)
	}
	deletePod(t, e, "reseed")
	waitContractSlots(t, e, rt, 0)

	events := rt.AdoptEvents()
	if len(events) < 6 {
		t.Fatalf("adopt events %d, want at least 6", len(events))
	}
	entropies := make([][]byte, len(events))
	samples := make([][]byte, len(events))
	for i, ev := range events {
		if len(ev.Entropy) != guest.AdoptEntropyLen || len(ev.Sample) != guest.AdoptSampleLen {
			t.Fatalf("event %d lengths entropy %d sample %d", i, len(ev.Entropy), len(ev.Sample))
		}
		entropies[i] = ev.Entropy
		samples[i] = ev.Sample
	}
	pairwiseDistinct(t, "host entropy", entropies)
	pairwiseDistinct(t, "guest sample", samples)
}

func TestAdoptReseedFailClosed(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		assertContractAdoptFailed(t, "error", "injected adopt failure")
	})
	t.Run("zero", func(t *testing.T) {
		assertContractAdoptFailed(t, "zero", "zero")
	})
	t.Run("duplicate", func(t *testing.T) {
		leaks := goleak.IgnoreCurrent()
		before := contractFDBefore(t)
		e, rt := contractEngine(t, simvz.Faults{Adopt: "duplicate"})
		defer finishContract(t, e, before, leaks)
		startPod(t, e, contractPod("keep", "uid-keep"))
		waitContractPhase(t, e, "keep", corev1.PodRunning, 15*time.Second)
		bad := contractPod("drop", "uid-drop")
		bad.Spec.Containers[0].Command = []string{"/bin/true"}
		startPod(t, e, bad)
		p := waitContractPhase(t, e, "drop", corev1.PodFailed, 15*time.Second)
		if !strings.Contains(p.Status.Message, "reused") {
			t.Fatalf("message %q", p.Status.Message)
		}
		if rt.WorkloadCalls("uid-drop") != 0 {
			t.Fatalf("workload calls %d", rt.WorkloadCalls("uid-drop"))
		}
		if !sawMethod(rt.Methods("uid-drop"), guest.MethodAdopt) {
			t.Fatalf("drop methods %v", rt.Methods("uid-drop"))
		}
		// Teardown stops the VM and SimVZ drops it from the machine list, so
		// the state is absent. Running would mean the failed start left it up.
		waitContractNotRunning(t, rt, "uid-drop")
		if rt.Running() != 1 {
			t.Fatalf("running %d, the accepted pod should still hold its VM", rt.Running())
		}
		deletePod(t, e, "keep")
		deletePod(t, e, "drop")
		waitContractSlots(t, e, rt, 0)
	})
}

func assertContractAdoptFailed(t *testing.T, mode, want string) {
	t.Helper()
	leaks := goleak.IgnoreCurrent()
	before := contractFDBefore(t)
	e, rt := contractEngine(t, simvz.Faults{Adopt: mode})
	defer finishContract(t, e, before, leaks)
	pod := contractPod("bad", "uid-bad")
	pod.Spec.Containers[0].Command = []string{"/bin/true"}
	startPod(t, e, pod)
	p := waitContractPhase(t, e, "bad", corev1.PodFailed, 15*time.Second)
	if !strings.Contains(p.Status.Message, want) {
		t.Fatalf("message %q, want %q", p.Status.Message, want)
	}
	if rt.WorkloadCalls("uid-bad") != 0 {
		t.Fatalf("workload calls %d", rt.WorkloadCalls("uid-bad"))
	}
	if !sawMethod(rt.Methods("uid-bad"), guest.MethodAdopt) {
		t.Fatalf("methods %v", rt.Methods("uid-bad"))
	}
	waitContractNotRunning(t, rt, "uid-bad")
	waitContractSlots(t, e, rt, 0)
}

func contractEngine(t *testing.T, faults simvz.Faults) (*engine.Engine, *simvz.Runtime) {
	t.Helper()
	leakcheck.Check(t)
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "simvz"
	cfg.CacheDir = t.TempDir()
	cfg.AllowNATWorkloads = true
	cfg.AgentReadyTimeout = 20 * time.Second
	cfg.ProbeInterval = time.Hour
	rt := simvz.New(simvz.Options{Dir: t.TempDir(), Faults: faults, MaxRunning: 2})
	e := engine.New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	return e, rt
}

func contractPod(name, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: k8stypes.UID(uid)},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "macos", Image: "local/macos:test"}},
		},
	}
}

func startPod(t *testing.T, e *engine.Engine, pod *corev1.Pod) {
	t.Helper()
	if err := e.Create(context.Background(), pod, engine.Credentials{}); err != nil {
		t.Fatal(err)
	}
}

func deletePod(t *testing.T, e *engine.Engine, name string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := e.Delete(ctx, "default", name, 0); err != nil {
		t.Errorf("delete %s: %v", name, err)
	}
}

func waitContractPhase(t *testing.T, e *engine.Engine, name string, phase corev1.PodPhase, budget time.Duration) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(budget)
	var last corev1.PodPhase
	var msg string
	for time.Now().Before(deadline) {
		p, err := e.Get("default", name)
		if err == nil {
			last = p.Status.Phase
			msg = p.Status.Message
			if last == phase {
				return p
			}
			if phase != corev1.PodFailed && last == corev1.PodFailed {
				t.Fatalf("pod %s failed: %s", name, msg)
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pod %s phase %s msg %q, want %s", name, last, msg, phase)
	return nil
}

func waitContractSlots(t *testing.T, e *engine.Engine, rt *simvz.Runtime, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if e.Slots().Used() == n && rt.Running() == n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("slots %d running %d, want %d", e.Slots().Used(), rt.Running(), n)
}

func waitContractNotRunning(t *testing.T, rt *simvz.Runtime, uid string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var got string
	for time.Now().Before(deadline) {
		got = rt.VMState(uid)
		// Stopped or Error is a VM the runtime still lists. Absent means
		// cleanup already removed it. Either way it is not running.
		if got == "Stopped" || got == "Error" || got == "absent" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("vm %s still %s", uid, got)
}

func sawMethod(methods []string, want string) bool {
	for _, m := range methods {
		if m == want {
			return true
		}
	}
	return false
}

func waitAdoptCount(t *testing.T, rt *simvz.Runtime, uid string, n int, budget time.Duration) {
	t.Helper()
	deadline := time.Now().Add(budget)
	var got int
	for time.Now().Before(deadline) {
		got = 0
		for _, ev := range rt.AdoptEvents() {
			if ev.UID == uid {
				got++
			}
		}
		if got >= n {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("uid %s adopt events %d, want %d", uid, got, n)
}

func pairwiseDistinct(t *testing.T, label string, vals [][]byte) {
	t.Helper()
	seen := map[string]int{}
	for i, v := range vals {
		if len(v) == 0 || allZero(v) {
			t.Fatalf("%s %d is empty or zero", label, i)
		}
		k := hex.EncodeToString(v)
		if j, ok := seen[k]; ok {
			t.Fatalf("%s %d duplicates %d", label, i, j)
		}
		seen[k] = i
	}
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func contractFDBefore(t *testing.T) int {
	t.Helper()
	n, err := simvz.FDCount()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func finishContract(t *testing.T, e *engine.Engine, before int, leaks goleak.Option) {
	t.Helper()
	defer goleak.VerifyNone(t, leaks)
	if e.Slots().Used() != 0 {
		t.Errorf("slots still held: %d", e.Slots().Used())
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		n, err := simvz.FDCount()
		if err != nil {
			t.Fatal(err)
		}
		if n <= before+2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("open fds %d, baseline %d", n, before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
