package node

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func conditionByType(conds []corev1.NodeCondition, typ corev1.NodeConditionType) (corev1.NodeCondition, bool) {
	for _, c := range conds {
		if c.Type == typ {
			return c, true
		}
	}
	return corev1.NodeCondition{}, false
}

// Node status is republished on a short period. LastTransitionTime has to
// stay put until the condition status changes, or a node looks perpetually
// fresh and pod minReadySeconds never elapses.
func TestNodeConditionTransitionTimeStaysUntilStatusChanges(t *testing.T) {
	inv := healthyInv()
	n := &corev1.Node{}
	if err := Apply(context.Background(), n, inv); err != nil {
		t.Fatal(err)
	}
	first := append([]corev1.NodeCondition(nil), n.Status.Conditions...)
	time.Sleep(20 * time.Millisecond)
	if err := Apply(context.Background(), n, inv); err != nil {
		t.Fatal(err)
	}
	for _, c := range n.Status.Conditions {
		prev, ok := conditionByType(first, c.Type)
		if !ok {
			t.Fatalf("condition %s disappeared", c.Type)
		}
		if prev.Status != c.Status {
			t.Fatalf("%s status changed %s -> %s with the same inventory", c.Type, prev.Status, c.Status)
		}
		if !c.LastTransitionTime.Time.Equal(prev.LastTransitionTime.Time) {
			t.Fatalf("%s LastTransitionTime moved from %s to %s while status stayed %s", c.Type, prev.LastTransitionTime.Format(time.RFC3339Nano), c.LastTransitionTime.Format(time.RFC3339Nano), c.Status)
		}
		if c.LastHeartbeatTime.Time.Equal(prev.LastHeartbeatTime.Time) {
			t.Fatalf("%s LastHeartbeatTime did not refresh", c.Type)
		}
	}

	inv.Host.DiskUsedFrac = 0.95
	time.Sleep(5 * time.Millisecond)
	if err := Apply(context.Background(), n, inv); err != nil {
		t.Fatal(err)
	}
	disk, ok := conditionByType(n.Status.Conditions, corev1.NodeDiskPressure)
	if !ok || disk.Status != corev1.ConditionTrue {
		t.Fatalf("DiskPressure %+v", disk)
	}
	prevDisk, _ := conditionByType(first, corev1.NodeDiskPressure)
	if !disk.LastTransitionTime.After(prevDisk.LastTransitionTime.Time) {
		t.Fatalf("DiskPressure LastTransitionTime %s did not move when status became True", disk.LastTransitionTime.Format(time.RFC3339Nano))
	}
	ready, _ := conditionByType(n.Status.Conditions, corev1.NodeReady)
	prevReady, _ := conditionByType(first, corev1.NodeReady)
	if !ready.LastTransitionTime.Time.Equal(prevReady.LastTransitionTime.Time) {
		t.Fatalf("NodeReady LastTransitionTime moved when only DiskPressure changed")
	}
}

// NotifyNodeStatus starts from a blank Node every period. The provider has
// to remember the conditions it published, or the transition time moves
// even when Apply would have kept it on one object.
func TestStatusProviderKeepsTransitionAcrossPublishes(t *testing.T) {
	p := &StatusProvider{inv: healthyInv()}
	first := p.nodeSnapshot()
	if first == nil || len(first.Status.Conditions) == 0 {
		t.Fatal("first snapshot has no conditions")
	}
	time.Sleep(20 * time.Millisecond)
	second := p.nodeSnapshot()
	if second == nil {
		t.Fatal("second snapshot")
	}
	for _, c := range second.Status.Conditions {
		prev, ok := conditionByType(first.Status.Conditions, c.Type)
		if !ok {
			t.Fatalf("condition %s disappeared", c.Type)
		}
		if !c.LastTransitionTime.Time.Equal(prev.LastTransitionTime.Time) {
			t.Fatalf("%s LastTransitionTime moved from %s to %s across publishes while status stayed %s", c.Type, prev.LastTransitionTime.Format(time.RFC3339Nano), c.LastTransitionTime.Format(time.RFC3339Nano), c.Status)
		}
		if c.LastHeartbeatTime.Time.Equal(prev.LastHeartbeatTime.Time) {
			t.Fatalf("%s LastHeartbeatTime did not refresh", c.Type)
		}
	}
}
