package engine

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// Adoption must hand the warm slot to the pod, not hold two slots for one VM.
func TestAdoptionReleasesWarmSlot(t *testing.T) {
	e, _, _ := warmEngine(t, 2, 1, warmImg)
	waitFor(t, "warm boot", func() bool { return e.warm.len() == 1 })

	if err := e.Create(context.Background(), samplePod("a1", "uid-a1"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "a1", corev1.PodRunning)

	// Invariant: every held slot is either the pod's or a live pool entry.
	// Before the fix the adopted entry's slot stayed held forever.
	assertSlotsAccounted(t, e)

	// The second pod must fit; a leaked warm slot would reject it.
	if err := e.Create(context.Background(), mountedPod("a2", "uid-a2", warmImg), secretCreds); err != nil {
		t.Fatalf("second pod rejected despite free capacity (slot leak): %v", err)
	}
	waitPhase(t, e, "default", "a2", corev1.PodRunning)
	assertSlotsAccounted(t, e)

	_ = e.Delete(context.Background(), "default", "a1", 0)
	_ = e.Delete(context.Background(), "default", "a2", 0)
	e.Close()
	if got := e.slots.Used(); got != 0 {
		t.Fatalf("slots leaked across pod lifetime: used=%d", got)
	}
}

// assertSlotsAccounted fails if any held slot belongs to neither a running
// pod nor an entry currently in the warm pool.
func assertSlotsAccounted(t *testing.T, e *Engine) {
	t.Helper()
	e.mu.RLock()
	podUIDs := map[string]bool{}
	for _, rec := range e.pods {
		rec.mu.Lock()
		podUIDs[string(rec.pod.UID)] = true
		rec.mu.Unlock()
	}
	e.mu.RUnlock()
	live := map[string]bool{}
	for _, entry := range e.warm.snapshot() {
		live[entry.slotID] = true
	}
	for _, uid := range e.slots.UIDs() {
		if !podUIDs[uid] && !live[uid] {
			t.Fatalf("slot %s held by neither pod nor pool (leak)", uid)
		}
	}
}
