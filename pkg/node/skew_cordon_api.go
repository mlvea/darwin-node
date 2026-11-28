package node

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

const (
	// AnnotationCordonedBy records who set spec.unschedulable. kubectl
	// cordon does not write it, so an operator cordon is distinct from
	// one this agent took for a supervisor wire-major mismatch.
	AnnotationCordonedBy = "darwin-node.io/cordoned-by"
	// CordonOwnerSupervisorSkew is the value this agent writes.
	CordonOwnerSupervisorSkew = "supervisor-skew"
	// ReasonSkewOperatorUncordon is the event reason when an operator
	// clears spec.unschedulable during a hold. The agent does not set
	// the bit again.
	ReasonSkewOperatorUncordon = "SupervisorSkewOperatorUncordon"

	skewNodeTimeout    = 10 * time.Second
	skewObserveTimeout = 2 * time.Second
)

// NodePatcher is the node read and patch used to cordon one node.
// Patch is the node object, not the status subresource.
type NodePatcher interface {
	Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Node, error)
	Patch(ctx context.Context, name string, pt types.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*corev1.Node, error)
}

// SetNodeAPI stores the reader used before the first cordon and before
// the last release. A nil reader keeps the in-memory priorUnschedulable
// argument.
func (s *SkewCordon) SetNodeAPI(name string, nodes NodePatcher) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.nodeName = name
	s.nodes = nodes
	s.mu.Unlock()
}

// SetNodeRequired means a missing reader must not be treated as a
// schedulable node. Cluster mode sets this: Recover runs before the
// process may have a client, and assuming schedulable would clear an
// operator cordon on the way out.
func (s *SkewCordon) SetNodeRequired(v bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.nodeRequired = v
	s.mu.Unlock()
}

// SetReporter receives yield once, when an operator clears the cordon
// during a hold. kind is "yield".
func (s *SkewCordon) SetReporter(fn func(kind, reason, message string)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.report = fn
	s.mu.Unlock()
}

// Reread repeats the first-hold decision. The rotation client does not
// exist until after Recover, so a hold may have assumed nothing until
// this runs.
func (s *SkewCordon) Reread(ctx context.Context) {
	if s == nil {
		return
	}
	s.apiMu.Lock()
	defer s.apiMu.Unlock()
	s.mu.Lock()
	if s.holds == 0 || s.nodes == nil || s.nodeName == "" || s.yielded {
		s.mu.Unlock()
		return
	}
	nodes, name := s.nodes, s.nodeName
	s.mu.Unlock()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, skewNodeTimeout)
	defer cancel()
	d, err := syncHoldCordon(ctx, nodes, name)
	if err != nil {
		slog.Warn("supervisor skew: node spec was not re-read; not claiming the cordon", "node", name, "err", err)
		s.mu.Lock()
		s.cordonedByUs = false
		s.mu.Unlock()
		return
	}
	s.mu.Lock()
	s.cordonedByUs = d.own && !d.yielded
	var fire bool
	var msg string
	var fn func(string, string, string)
	if d.yielded {
		fire, msg = s.markYield(d.message)
		fn = s.report
	}
	s.mu.Unlock()
	if fire && fn != nil {
		fn("yield", ReasonSkewOperatorUncordon, msg)
	}
}

// observeOperator notices an uncordon that landed while the hold is
// still up. Status publishes call Apply on a timer; fighting that
// uncordon would set spec.unschedulable again on every pass.
func (s *SkewCordon) observeOperator() {
	if s == nil || !s.apiMu.TryLock() {
		return
	}
	defer s.apiMu.Unlock()
	s.mu.Lock()
	if s.nodes == nil || s.nodeName == "" || s.holds == 0 || s.yielded {
		s.mu.Unlock()
		return
	}
	nodes, name := s.nodes, s.nodeName
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), skewObserveTimeout)
	defer cancel()
	cleared, err := readOperatorUncordon(ctx, nodes, name)
	if err != nil || !cleared {
		return
	}
	s.mu.Lock()
	fire, msg := s.markYield(operatorUncordonMessage())
	fn := s.report
	s.mu.Unlock()
	if fire && fn != nil {
		fn("yield", ReasonSkewOperatorUncordon, msg)
	}
}

// decideHold reads the API node when a client is attached. The bools
// are ownership and "operator already uncordoned".
func (s *SkewCordon) decideHold(priorUnschedulable bool) (owned, yielded bool, msg string) {
	s.mu.Lock()
	nodes, name, required := s.nodes, s.nodeName, s.nodeRequired
	s.mu.Unlock()
	if nodes != nil && name != "" {
		ctx, cancel := context.WithTimeout(context.Background(), skewNodeTimeout)
		defer cancel()
		d, err := syncHoldCordon(ctx, nodes, name)
		if err != nil {
			slog.Warn("supervisor skew: node spec was not read; not claiming the cordon", "node", name, "err", err)
			return false, false, ""
		}
		return d.own, d.yielded, d.message
	}
	if required {
		slog.Warn("supervisor skew: node spec was not read; not claiming the cordon", "node", name)
		return false, false, ""
	}
	return !priorUnschedulable, false, ""
}

// finishRelease re-reads before clearing. False means Apply must leave
// spec.unschedulable alone.
func (s *SkewCordon) finishRelease(owned bool) bool {
	s.mu.Lock()
	nodes, name := s.nodes, s.nodeName
	s.mu.Unlock()
	if nodes == nil || name == "" {
		return owned
	}
	ctx, cancel := context.WithTimeout(context.Background(), skewNodeTimeout)
	defer cancel()
	action, err := syncReleaseCordon(ctx, nodes, name, owned)
	if err != nil {
		slog.Warn("supervisor skew: node spec was not re-read; leaving the cordon", "node", name, "err", err)
		s.mu.Lock()
		s.cordonedByUs = false
		s.mu.Unlock()
		return false
	}
	switch action {
	case actionCleared:
		s.mu.Lock()
		s.cordonedByUs = true
		s.yielded = false
		s.mu.Unlock()
		return true
	case actionYielded:
		s.mu.Lock()
		fire, msg := s.markYield(operatorUncordonMessage())
		fn := s.report
		s.mu.Unlock()
		if fire && fn != nil {
			fn("yield", ReasonSkewOperatorUncordon, msg)
		}
		return false
	default:
		s.mu.Lock()
		s.cordonedByUs = false
		s.mu.Unlock()
		return false
	}
}

// markYield records one operator-uncordon. s.mu is held by the caller.
func (s *SkewCordon) markYield(msg string) (bool, string) {
	s.yielded = true
	s.cordonedByUs = false
	if msg == "" {
		msg = operatorUncordonMessage()
	}
	if s.yieldReported {
		return false, msg
	}
	s.yieldReported = true
	s.events = append(s.events, SkewEvent{Kind: "yield", Reason: ReasonSkewOperatorUncordon, Message: msg})
	return true, msg
}

func operatorUncordonMessage() string {
	return "operator cleared spec.unschedulable during a supervisor version-skew hold; not cordoning again"
}

type holdDecision struct {
	own     bool
	yielded bool
	message string
}

type cordonAction int

const (
	actionKept cordonAction = iota
	actionCleared
	actionYielded
)

func syncHoldCordon(ctx context.Context, nodes NodePatcher, name string) (holdDecision, error) {
	var d holdDecision
	err := retry.OnError(retry.DefaultRetry, retryablePatch, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cur, err := nodes.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			d = holdDecision{own: true}
			return nil
		}
		if err != nil {
			return err
		}
		ours := annotationOurs(cur)
		if !cur.Spec.Unschedulable {
			if ours {
				if err := patchDropAnnotation(ctx, nodes, name, cur); err != nil {
					return err
				}
				d = holdDecision{yielded: true, message: operatorUncordonMessage()}
				return nil
			}
			if err := patchCordon(ctx, nodes, name, cur); err != nil {
				return err
			}
			d = holdDecision{own: true}
			return nil
		}
		if ours {
			d = holdDecision{own: true}
			return nil
		}
		d = holdDecision{}
		return nil
	})
	if err != nil {
		return holdDecision{}, err
	}
	return d, nil
}

func syncReleaseCordon(ctx context.Context, nodes NodePatcher, name string, owned bool) (cordonAction, error) {
	var action cordonAction
	err := retry.OnError(retry.DefaultRetry, retryablePatch, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cur, err := nodes.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if owned {
				action = actionCleared
			} else {
				action = actionKept
			}
			return nil
		}
		if err != nil {
			return err
		}
		ours := annotationOurs(cur)
		if !cur.Spec.Unschedulable {
			if err := patchDropAnnotation(ctx, nodes, name, cur); err != nil {
				return err
			}
			if ours || owned {
				action = actionYielded
			} else {
				action = actionKept
			}
			return nil
		}
		if !ours {
			action = actionKept
			return nil
		}
		if err := patchClear(ctx, nodes, name, cur); err != nil {
			return err
		}
		action = actionCleared
		return nil
	})
	if err != nil {
		return actionKept, err
	}
	return action, nil
}

// readOperatorUncordon reports a schedulable API node. Our annotation
// is removed so a later read does not treat a cleared cordon as owned.
// spec.unschedulable is not written.
func readOperatorUncordon(ctx context.Context, nodes NodePatcher, name string) (bool, error) {
	var cleared bool
	err := retry.OnError(retry.DefaultRetry, retryablePatch, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cur, err := nodes.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			cleared = false
			return nil
		}
		if err != nil {
			return err
		}
		if cur.Spec.Unschedulable {
			cleared = false
			return nil
		}
		if err := patchDropAnnotation(ctx, nodes, name, cur); err != nil {
			return err
		}
		cleared = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return cleared, nil
}

func annotationOurs(n *corev1.Node) bool {
	return n != nil && n.Annotations[AnnotationCordonedBy] == CordonOwnerSupervisorSkew
}

// patchCordon sets the bit and the annotation. A JSON patch is used
// because a merge patch is applied again onto whatever object is
// current, which would stamp our annotation onto an operator cordon
// that landed after the read. The resourceVersion test fails the
// whole patch instead.
func patchCordon(ctx context.Context, nodes NodePatcher, name string, cur *corev1.Node) error {
	ops := rvTest(cur)
	if len(cur.Annotations) == 0 {
		ops = append(ops, map[string]any{
			"op":   "add",
			"path": "/metadata/annotations",
			"value": map[string]string{
				AnnotationCordonedBy: CordonOwnerSupervisorSkew,
			},
		})
	} else {
		ops = append(ops, map[string]any{
			"op":    "add",
			"path":  "/metadata/annotations/" + pointerEscape(AnnotationCordonedBy),
			"value": CordonOwnerSupervisorSkew,
		})
	}
	// spec is omitted from JSON while it is empty. Adding the child
	// of a missing parent fails, so the empty object goes on first.
	if !specInJSON(cur) {
		ops = append(ops, map[string]any{
			"op":    "add",
			"path":  "/spec",
			"value": map[string]any{},
		})
	}
	ops = append(ops, map[string]any{
		"op":    "add",
		"path":  "/spec/unschedulable",
		"value": true,
	})
	return applyJSONPatch(ctx, nodes, name, ops)
}

func patchClear(ctx context.Context, nodes NodePatcher, name string, cur *corev1.Node) error {
	ops := rvTest(cur)
	ops = append(ops, map[string]any{
		"op":    "add",
		"path":  "/spec/unschedulable",
		"value": false,
	})
	if annotationOurs(cur) {
		ops = append(ops, map[string]any{
			"op":   "remove",
			"path": "/metadata/annotations/" + pointerEscape(AnnotationCordonedBy),
		})
	}
	return applyJSONPatch(ctx, nodes, name, ops)
}

func patchDropAnnotation(ctx context.Context, nodes NodePatcher, name string, cur *corev1.Node) error {
	if !annotationOurs(cur) {
		return nil
	}
	ops := rvTest(cur)
	ops = append(ops, map[string]any{
		"op":   "remove",
		"path": "/metadata/annotations/" + pointerEscape(AnnotationCordonedBy),
	})
	return applyJSONPatch(ctx, nodes, name, ops)
}

func applyJSONPatch(ctx context.Context, nodes NodePatcher, name string, ops []map[string]any) error {
	if len(ops) == 0 {
		return nil
	}
	body, err := json.Marshal(ops)
	if err != nil {
		return err
	}
	_, err = nodes.Patch(ctx, name, types.JSONPatchType, body, metav1.PatchOptions{})
	return err
}

func rvTest(cur *corev1.Node) []map[string]any {
	if cur == nil || cur.ResourceVersion == "" {
		// The fake clientset leaves resourceVersion empty. Testing a
		// path that omitempty dropped would fail the patch.
		return nil
	}
	return []map[string]any{{
		"op":    "test",
		"path":  "/metadata/resourceVersion",
		"value": cur.ResourceVersion,
	}}
}

func specInJSON(cur *corev1.Node) bool {
	if cur == nil {
		return false
	}
	raw, err := json.Marshal(cur.Spec)
	if err != nil {
		return true
	}
	return string(raw) != "{}"
}

func pointerEscape(s string) string {
	s = strings.ReplaceAll(s, "~", "~0")
	s = strings.ReplaceAll(s, "/", "~1")
	return s
}

// retryablePatch matches a conflict and a JSON-patch test failure.
// The apiserver returns 422 for a failed test. The fake clientset
// returns the patch library's plain "test failed" error.
func retryablePatch(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsConflict(err) {
		return true
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Code == http.StatusUnprocessableEntity {
		return true
	}
	return strings.Contains(err.Error(), "test failed")
}
