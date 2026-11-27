package node

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestSkewCordonHoldAndRelease(t *testing.T) {
	s := &SkewCordon{}
	s.Release()
	n := &corev1.Node{}
	s.Apply(n)
	if n.Spec.Unschedulable {
		t.Fatal("release with no hold cordoned the node")
	}

	const reason = "supervisor wire major 2; node-agent wire major 1; VM left running"
	s.Hold(false, reason)
	s.Hold(false, "second")
	n = &corev1.Node{}
	s.Apply(n)
	if !n.Spec.Unschedulable {
		t.Fatal("unschedulable")
	}
	c, ok := conditionByType(n.Status.Conditions, ConditionSupervisorSkew)
	if !ok || c.Status != corev1.ConditionTrue || c.Reason != ReasonSupervisorSkew || !strings.Contains(c.Message, "wire major 2") {
		t.Fatalf("condition %+v", c)
	}
	first := c.LastTransitionTime
	n2 := &corev1.Node{}
	s.Apply(n2)
	c2, _ := conditionByType(n2.Status.Conditions, ConditionSupervisorSkew)
	if !c2.LastTransitionTime.Equal(&first) {
		t.Fatalf("transition moved %s -> %s", first, c2.LastTransitionTime)
	}
	if s.Release() {
		t.Fatal("one of two holds cleared the cordon")
	}
	n = &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}}
	s.Apply(n)
	if !n.Spec.Unschedulable {
		t.Fatal("cordon dropped while a hold remained")
	}
	if !s.Release() {
		t.Fatal("last hold did not uncordon")
	}
	n = &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}}
	s.Apply(n)
	if n.Spec.Unschedulable {
		t.Fatal("unschedulable stayed set")
	}
	if _, ok := conditionByType(n.Status.Conditions, ConditionSupervisorSkew); ok {
		t.Fatal("condition remained")
	}
	// A later publish must not clear an operator cordon.
	n = &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}}
	s.Apply(n)
	if !n.Spec.Unschedulable {
		t.Fatal("operator cordon was cleared")
	}
	ev := s.Events()
	if len(ev) != 2 || ev[0].Kind != "cordon" || ev[1].Kind != "clear" {
		t.Fatalf("events %+v", ev)
	}
}

func TestSkewCordonLeavesAnOperatorCordon(t *testing.T) {
	s := &SkewCordon{}
	s.Hold(true, "supervisor has no version handshake (wire major 0); this node-agent speaks wire major 1; the VM was left running")
	n := &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}}
	s.Apply(n)
	if !n.Spec.Unschedulable {
		t.Fatal("active hold cleared unschedulable")
	}
	if _, ok := conditionByType(n.Status.Conditions, ConditionSupervisorSkew); !ok {
		t.Fatal("missing condition")
	}
	if s.Release() {
		t.Fatal("operator cordon was reported as ours")
	}
	n = &corev1.Node{Spec: corev1.NodeSpec{Unschedulable: true}}
	s.Apply(n)
	if !n.Spec.Unschedulable {
		t.Fatal("operator cordon cleared")
	}
	if _, ok := conditionByType(n.Status.Conditions, ConditionSupervisorSkew); ok {
		t.Fatal("condition remained after release")
	}
}

func TestStatusProviderPublishesSkewCordon(t *testing.T) {
	p := NewStatusProvider(healthyInv())
	s := &SkewCordon{}
	p.SetSkewCordon(s)
	s.Hold(false, "supervisor wire major 2; node-agent wire major 1; VM left running")
	n := p.nodeSnapshot()
	if n == nil || !n.Spec.Unschedulable {
		t.Fatalf("snapshot %+v", n)
	}
	if _, ok := conditionByType(n.Status.Conditions, ConditionSupervisorSkew); !ok {
		t.Fatal("snapshot dropped the condition")
	}
	// A second publish rebuilds conditions. The cordon has to be put back.
	again := p.nodeSnapshot()
	if again == nil || !again.Spec.Unschedulable {
		t.Fatal("second snapshot lost the cordon")
	}
	if _, ok := conditionByType(again.Status.Conditions, ConditionSupervisorSkew); !ok {
		t.Fatal("second snapshot dropped the condition")
	}
	s.Release()
	cleared := p.nodeSnapshot()
	if cleared == nil || cleared.Spec.Unschedulable {
		t.Fatal("snapshot stayed unschedulable")
	}
	if _, ok := conditionByType(cleared.Status.Conditions, ConditionSupervisorSkew); ok {
		t.Fatal("condition survived release")
	}
}
