package node

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// ConditionSupervisorSkew is set while a supervisor's wire major does
	// not match this node-agent. The VM is left running.
	ConditionSupervisorSkew corev1.NodeConditionType = "SupervisorVersionSkew"
	// ReasonSupervisorSkew is the condition and event reason.
	ReasonSupervisorSkew = "WireMajorMismatch"
)

// SkewEvent is one cordon or clear recorded for tests and the engine.
type SkewEvent struct {
	Kind    string
	Reason  string
	Message string
}

// SkewCordon marks the node unschedulable while at least one supervisor
// is held for a wire-major mismatch. Unschedulable is cleared only when
// the last hold releases and this process is the one that cordoned.
// An operator cordon (priorUnschedulable) is left in place.
type SkewCordon struct {
	mu           sync.Mutex
	holds        int
	cordonedByUs bool
	reason       string
	since        metav1.Time
	events       []SkewEvent
}

// Hold counts one mismatched supervisor. priorUnschedulable is the
// node's spec at the moment of the hold. A blank status snapshot is not
// a usable signal, so the caller passes it explicitly. The first hold
// decides ownership; later holds only keep the node cordoned.
func (s *SkewCordon) Hold(priorUnschedulable bool, reason string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds == 0 {
		s.cordonedByUs = !priorUnschedulable
		s.since = metav1.Now()
		s.reason = reason
		s.events = append(s.events, SkewEvent{Kind: "cordon", Reason: ReasonSupervisorSkew, Message: reason})
	}
	s.holds++
}

// Release drops one hold. The returned bool is true when this release
// removed the last hold and darwin-node itself cordoned the node. Apply
// is what clears spec.unschedulable; a release with holds still above
// zero leaves the cordon up.
func (s *SkewCordon) Release() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds == 0 {
		return false
	}
	s.holds--
	if s.holds > 0 {
		return false
	}
	s.events = append(s.events, SkewEvent{Kind: "clear", Reason: ReasonSupervisorSkew, Message: s.reason})
	return s.cordonedByUs
}

// Apply writes the cordon onto n. A status publisher builds a blank node
// on every pass, so this has to run after that node is filled in. The
// condition's transition time stays at the first hold.
func (s *SkewCordon) Apply(n *corev1.Node) {
	if s == nil || n == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds > 0 {
		n.Spec.Unschedulable = true
		s.upsertLocked(n)
		return
	}
	s.removeLocked(n)
	if s.cordonedByUs {
		n.Spec.Unschedulable = false
		s.cordonedByUs = false
	}
}

// Events is a copy of the cordon and clear records.
func (s *SkewCordon) Events() []SkewEvent {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SkewEvent, len(s.events))
	copy(out, s.events)
	return out
}

func (s *SkewCordon) upsertLocked(n *corev1.Node) {
	now := metav1.Now()
	msg := s.reason
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type != ConditionSupervisorSkew {
			continue
		}
		n.Status.Conditions[i].Status = corev1.ConditionTrue
		n.Status.Conditions[i].Reason = ReasonSupervisorSkew
		n.Status.Conditions[i].Message = msg
		n.Status.Conditions[i].LastHeartbeatTime = now
		if n.Status.Conditions[i].LastTransitionTime.IsZero() {
			n.Status.Conditions[i].LastTransitionTime = s.since
		}
		return
	}
	n.Status.Conditions = append(n.Status.Conditions, cond(ConditionSupervisorSkew, corev1.ConditionTrue, s.since, ReasonSupervisorSkew, msg))
}

func (s *SkewCordon) removeLocked(n *corev1.Node) {
	out := n.Status.Conditions[:0]
	for _, c := range n.Status.Conditions {
		if c.Type == ConditionSupervisorSkew {
			continue
		}
		out = append(out, c)
	}
	n.Status.Conditions = out
}
