package engine

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// A Deployment with minReadySeconds counts from PodReady's
// LastTransitionTime. Virtual-kubelet reads status every few seconds, so
// rewriting that timestamp on every read keeps the pod "just became Ready".
func TestPodConditionTransitionTimeStaysUntilStatusChanges(t *testing.T) {
	e := testEngine(t)
	if err := e.Create(context.Background(), samplePod("p", "uid-trans"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Delete(context.Background(), "default", "p", 0) })
	waitPhase(t, e, "default", "p", corev1.PodRunning)
	deadline := time.Now().Add(3 * time.Second)
	var ready corev1.PodCondition
	for time.Now().Before(deadline) {
		p, err := e.Get("default", "p")
		if err != nil {
			t.Fatal(err)
		}
		c, ok := cond(p, corev1.PodReady)
		if ok && c.Status == corev1.ConditionTrue {
			ready = c
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ready.Status != corev1.ConditionTrue {
		t.Fatal("pod did not become Ready")
	}
	p, err := e.Get("default", "p")
	if err != nil {
		t.Fatal(err)
	}
	scheduled, ok := cond(p, corev1.PodScheduled)
	if !ok {
		t.Fatal("missing PodScheduled")
	}
	time.Sleep(20 * time.Millisecond)
	p, err = e.Get("default", "p")
	if err != nil {
		t.Fatal(err)
	}
	ready2, ok := cond(p, corev1.PodReady)
	if !ok || ready2.Status != corev1.ConditionTrue {
		t.Fatalf("PodReady %+v", ready2)
	}
	if !ready2.LastTransitionTime.Equal(&ready.LastTransitionTime) && !ready2.LastTransitionTime.Time.Equal(ready.LastTransitionTime.Time) {
		t.Fatalf("PodReady LastTransitionTime moved from %s to %s while Ready stayed True", ready.LastTransitionTime.Format(time.RFC3339Nano), ready2.LastTransitionTime.Format(time.RFC3339Nano))
	}
	scheduled2, _ := cond(p, corev1.PodScheduled)
	if !scheduled2.LastTransitionTime.Time.Equal(scheduled.LastTransitionTime.Time) {
		t.Fatalf("PodScheduled LastTransitionTime moved from %s to %s", scheduled.LastTransitionTime.Format(time.RFC3339Nano), scheduled2.LastTransitionTime.Format(time.RFC3339Nano))
	}

	e.mu.RLock()
	rec := e.pods[Key("default", "p")]
	e.mu.RUnlock()
	rec.mu.Lock()
	rec.ready = false
	rec.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	p, err = e.Get("default", "p")
	if err != nil {
		t.Fatal(err)
	}
	ready3, _ := cond(p, corev1.PodReady)
	if ready3.Status != corev1.ConditionFalse {
		t.Fatalf("PodReady status %s, want False after the container left Ready", ready3.Status)
	}
	if !ready3.LastTransitionTime.After(ready2.LastTransitionTime.Time) {
		t.Fatalf("PodReady LastTransitionTime %s did not move when Ready became False (was %s)", ready3.LastTransitionTime.Format(time.RFC3339Nano), ready2.LastTransitionTime.Format(time.RFC3339Nano))
	}
	scheduled3, _ := cond(p, corev1.PodScheduled)
	if !scheduled3.LastTransitionTime.Time.Equal(scheduled.LastTransitionTime.Time) {
		t.Fatalf("PodScheduled LastTransitionTime moved when only PodReady changed")
	}
}
