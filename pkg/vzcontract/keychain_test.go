package vzcontract

import (
	"context"
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
)

// TestConsecutivePodsOnOneSlotDoNotShareKeychain boots two pods one after
// another on a one-slot node. Each gets its own job keychain. Ending the
// pod removes that file. The System keychain stays.
func TestConsecutivePodsOnOneSlotDoNotShareKeychain(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	before := contractFDBefore(t)
	leakcheck.Check(t)
	slots, err := capacity.New(1)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "simvz"
	cfg.CacheDir = t.TempDir()
	cfg.AllowNATWorkloads = true
	cfg.AgentReadyTimeout = 20 * time.Second
	cfg.ProbeInterval = time.Hour
	rt := simvz.New(simvz.Options{Dir: t.TempDir(), MaxRunning: 1})
	e := engine.New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	defer finishContract(t, e, before, leaks)

	startPod(t, e, contractPod("slot-a", "uid-slot-a"))
	waitContractPhase(t, e, "slot-a", corev1.PodRunning, 15*time.Second)
	pathA := jobKeychainPath(t, rt, "uid-slot-a")
	deletePodGrace(t, e, "slot-a", 1)
	if rt.Keychain("uid-slot-a").Exists(pathA) {
		t.Fatal("pod a keychain still present after the pod ended")
	}
	if sys, err := rt.Keychain("uid-slot-a").SystemPath(); err != nil || !rt.Keychain("uid-slot-a").Exists(sys) {
		t.Fatalf("system keychain after pod a: %q %v", sys, err)
	}
	waitContractSlots(t, e, rt, 0)

	startPod(t, e, contractPod("slot-b", "uid-slot-b"))
	waitContractPhase(t, e, "slot-b", corev1.PodRunning, 15*time.Second)
	pathB := jobKeychainPath(t, rt, "uid-slot-b")
	if pathA == pathB {
		t.Fatalf("consecutive pods on one slot share keychain state %s", pathA)
	}
	if !strings.HasSuffix(pathA, "darwin-node-job-uid-slot-a.keychain-db") || !strings.HasSuffix(pathB, "darwin-node-job-uid-slot-b.keychain-db") {
		t.Fatalf("paths %s %s", pathA, pathB)
	}
	deletePodGrace(t, e, "slot-b", 1)
	if rt.Keychain("uid-slot-b").Exists(pathB) {
		t.Fatal("pod b keychain still present after the pod ended")
	}
	waitContractSlots(t, e, rt, 0)
}

func TestEphemeralKeychainCreateFailsClosed(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	before := contractFDBefore(t)
	e, rt := contractEngine(t, simvz.Faults{Keychain: "create"})
	defer finishContract(t, e, before, leaks)
	startPod(t, e, contractPod("bad", "uid-bad-kc"))
	p := waitContractPhase(t, e, "bad", corev1.PodFailed, 15*time.Second)
	if !strings.Contains(p.Status.Message, "ephemeral keychain: create") {
		t.Fatalf("message %q", p.Status.Message)
	}
	if rt.WorkloadCalls("uid-bad-kc") != 0 {
		t.Fatalf("workload calls %d", rt.WorkloadCalls("uid-bad-kc"))
	}
	if !sawMethod(rt.Methods("uid-bad-kc"), guest.MethodEphemeralKeychain) {
		t.Fatalf("methods %v", rt.Methods("uid-bad-kc"))
	}
	if sawMethod(rt.Methods("uid-bad-kc"), guest.MethodWorkload) {
		t.Fatalf("workload ran after keychain failure: %v", rt.Methods("uid-bad-kc"))
	}
	waitContractNotRunning(t, rt, "uid-bad-kc")
	waitContractSlots(t, e, rt, 0)
}

func jobKeychainPath(t *testing.T, rt *simvz.Runtime, uid string) string {
	t.Helper()
	kc := rt.Keychain(uid)
	if kc == nil {
		t.Fatalf("no in-memory keychain for %s", uid)
	}
	def, err := kc.Default()
	if err != nil {
		t.Fatal(err)
	}
	home, err := kc.Home()
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := kc.LoginHidden(home)
	if err != nil || !hidden {
		t.Fatalf("login hidden %v %v", hidden, err)
	}
	list, err := kc.UserSearchList()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0] != def {
		t.Fatalf("user search list %v default %s", list, def)
	}
	if !kc.NoAutoLock(def) {
		t.Fatal("job keychain auto-locks")
	}
	sys, err := kc.SystemPath()
	if err != nil || !kc.Exists(sys) {
		t.Fatalf("system keychain %q %v", sys, err)
	}
	return def
}

func deletePodGrace(t *testing.T, e *engine.Engine, name string, grace int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := e.Delete(ctx, "default", name, grace); err != nil {
		t.Fatalf("delete %s: %v", name, err)
	}
}
