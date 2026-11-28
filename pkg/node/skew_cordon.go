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
// is held for a wire-major mismatch. Ownership is the annotation
// darwin-node.io/cordoned-by=supervisor-skew on the API node.
// spec.unschedulable is cleared only when the last hold releases and
// that annotation is still ours. An operator cordon is left in place.
//
// apiMu covers the node read and patch. mu covers the counters.
// Lock order is apiMu then mu.
type SkewCordon struct {
	mu            sync.Mutex
	apiMu         sync.Mutex
	holds         int
	cordonedByUs  bool
	yielded       bool
	yieldReported bool
	nodeRequired  bool
	reason        string
	since         metav1.Time
	events        []SkewEvent
	nodes         NodePatcher
	nodeName      string
	report        func(kind, reason, message string)
}

// Hold counts one mismatched supervisor. priorUnschedulable is used
// only when no node client is attached. The first hold reads the API
// node and decides ownership; later holds keep the cordon up.
func (s *SkewCordon) Hold(priorUnschedulable bool, reason string) {
	if s == nil {
		return
	}
	s.apiMu.Lock()
	defer s.apiMu.Unlock()
	s.mu.Lock()
	first := s.holds == 0
	s.holds++
	if !first {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	owned, yielded, msg := s.decideHold(priorUnschedulable)
	s.mu.Lock()
	s.since = metav1.Now()
	s.reason = reason
	var fire bool
	var fn func(string, string, string)
	if yielded {
		fire, msg = s.markYield(msg)
		fn = s.report
	} else {
		s.cordonedByUs = owned
		s.events = append(s.events, SkewEvent{Kind: "cordon", Reason: ReasonSupervisorSkew, Message: reason})
	}
	s.mu.Unlock()
	if fire && fn != nil {
		fn("yield", ReasonSkewOperatorUncordon, msg)
	}
}

// Release drops one hold. The returned bool is true when this release
// removed the last hold and the API node still carries our annotation,
// so Apply clears spec.unschedulable. A release with holds still above
// zero leaves the cordon up.
func (s *SkewCordon) Release() bool {
	if s == nil {
		return false
	}
	s.apiMu.Lock()
	defer s.apiMu.Unlock()
	s.mu.Lock()
	if s.holds == 0 {
		s.mu.Unlock()
		return false
	}
	s.holds--
	if s.holds > 0 {
		s.mu.Unlock()
		return false
	}
	s.events = append(s.events, SkewEvent{Kind: "clear", Reason: ReasonSupervisorSkew, Message: s.reason})
	owned := s.cordonedByUs && !s.yielded
	s.mu.Unlock()
	return s.finishRelease(owned)
}

// Apply writes the cordon onto n. A status publisher builds a blank node
// on every pass, so this has to run after that node is filled in. The
// condition's transition time stays at the first hold. The ownership
// annotation is not copied here: the node controller merges annotations
// from this snapshot, and writing it back would replace an operator's.
func (s *SkewCordon) Apply(n *corev1.Node) {
	if s == nil || n == nil {
		return
	}
	s.observeOperator()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holds > 0 {
		if !s.yielded {
			n.Spec.Unschedulable = true
		}
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
