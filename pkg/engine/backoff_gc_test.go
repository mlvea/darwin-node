package engine

import (
	"context"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime/fake"

	corev1 "k8s.io/api/core/v1"
)

// Delete has to drop the crash-loop backoff entry. The map is keyed by pod
// UID, so a later pod with that UID would otherwise wait out the previous
// pod's delay, and entries for deleted pods would accumulate for the life
// of the process. client-go GC only drops entries that have been stable for
// twice the cap, so a fresh entry survives GC.
func TestDeleteDropsSidecarBackoff(t *testing.T) {
	rt := fake.New()
	sc := newRestartingSidecar()
	sc.exitAlways["log"] = 1
	e := sidecarEngine(t, rt, sc)
	const uid = "uid-bo-gc"
	if err := e.Create(context.Background(), hybridPod("hy", uid, corev1.RestartPolicyAlways), Credentials{}); err != nil {
		t.Fatal(err)
	}

	key := uid + "/log"
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if sc.createsOf("log") >= 2 && e.sidecarBackOff.Get(key) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e.sidecarBackOff.Get(key) == 0 {
		t.Fatalf("backoff was not armed, creates=%d", sc.createsOf("log"))
	}

	if err := e.Delete(context.Background(), "default", "hy", 0); err != nil {
		t.Fatal(err)
	}
	if d := e.sidecarBackOff.Get(key); d != 0 {
		t.Fatalf("backoff entry survived pod delete: %s", d)
	}

	before := sc.createsOf("log")
	if err := e.Create(context.Background(), hybridPod("hy", uid, corev1.RestartPolicyAlways), Credentials{}); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if sc.createsOf("log") >= before+2 {
			if e.slots.Used() != 1 || rt.Running() != 1 {
				t.Fatalf("slots=%d running=%d", e.slots.Used(), rt.Running())
			}
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("same UID reused the deleted pod's backoff: creates %d -> %d", before, sc.createsOf("log"))
}
