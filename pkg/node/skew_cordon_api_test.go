package node

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
)

// conflictOnce lets the first cordon patch lose to an operator cordon.
// The stored node becomes unschedulable with no skew annotation, and the
// patch returns a conflict so the caller has to re-read.
type conflictOnce struct {
	mu         sync.Mutex
	inner      NodePatcher
	gets       int
	conflicted bool
	firstPatch []byte
}

func (c *conflictOnce) Get(ctx context.Context, name string, opts metav1.GetOptions) (*corev1.Node, error) {
	c.mu.Lock()
	c.gets++
	c.mu.Unlock()
	return c.inner.Get(ctx, name, opts)
}

func (c *conflictOnce) Patch(ctx context.Context, name string, pt k8stypes.PatchType, data []byte, opts metav1.PatchOptions, subresources ...string) (*corev1.Node, error) {
	c.mu.Lock()
	first := !c.conflicted
	c.conflicted = true
	c.mu.Unlock()
	if !first {
		return c.inner.Patch(ctx, name, pt, data, opts, subresources...)
	}
	c.mu.Lock()
	c.firstPatch = append([]byte(nil), data...)
	c.mu.Unlock()
	cur, err := c.inner.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	cur.Spec.Unschedulable = true
	delete(cur.Annotations, AnnotationCordonedBy)
	if _, err := c.updater().Update(ctx, cur, metav1.UpdateOptions{}); err != nil {
		return nil, err
	}
	return nil, apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, name, fmt.Errorf("operator cordon"))
}

type nodeUpdater interface {
	Update(context.Context, *corev1.Node, metav1.UpdateOptions) (*corev1.Node, error)
}

func (c *conflictOnce) updater() nodeUpdater {
	return c.inner.(nodeUpdater)
}

func TestSkewHoldRetriesPatchConflict(t *testing.T) {
	const name = "n"
	cs := k8sfake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "7"}})
	p := &conflictOnce{inner: cs.CoreV1().Nodes()}
	s := &SkewCordon{}
	s.SetNodeAPI(name, p)
	s.Hold(false, "supervisor wire major 2; node-agent wire major 1; VM left running")
	if p.gets < 2 {
		t.Fatalf("gets %d, conflict was not re-read", p.gets)
	}
	p.mu.Lock()
	body := append([]byte(nil), p.firstPatch...)
	p.mu.Unlock()
	if !bytes.Contains(body, []byte(`"op":"test"`)) || !bytes.Contains(body, []byte(`"7"`)) {
		t.Fatalf("patch did not test resourceVersion: %s", body)
	}
	got, err := cs.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Unschedulable {
		t.Fatal("operator cordon was lost")
	}
	if got.Annotations[AnnotationCordonedBy] == CordonOwnerSupervisorSkew {
		t.Fatal("conflict retry claimed the operator cordon")
	}
	if s.Release() {
		t.Fatal("release cleared a cordon this process does not own")
	}
	got, err = cs.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Unschedulable {
		t.Fatal("release cleared the operator cordon")
	}
}

func TestLateNodeReadDropsAssumedOwnership(t *testing.T) {
	const name = "late"
	cs := k8sfake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{Unschedulable: true},
	})
	s := &SkewCordon{}
	s.Hold(false, "supervisor wire major 2")
	s.SetNodeAPI(name, cs.CoreV1().Nodes())
	s.Reread(context.Background())
	if s.Release() {
		t.Fatal("a late read still treated the operator cordon as ours")
	}
	got, err := cs.CoreV1().Nodes().Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Spec.Unschedulable {
		t.Fatal("late read cleared the operator cordon")
	}
	if got.Annotations[AnnotationCordonedBy] == CordonOwnerSupervisorSkew {
		t.Fatal("late read claimed the operator cordon")
	}
}

func TestRequiredNodeReadDoesNotAssumeSchedulable(t *testing.T) {
	s := &SkewCordon{}
	s.SetNodeRequired(true)
	s.Hold(false, "supervisor wire major 2")
	if s.Release() {
		t.Fatal("claimed the cordon without reading the node")
	}
}
