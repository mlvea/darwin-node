package node

import (
	"context"
	"testing"

	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

const (
	wantOSKey   = "darwin.node/os"
	wantOSValue = "macos"
)

func taintByKey(taints []corev1.Taint, key string) (corev1.Taint, bool) {
	for _, taint := range taints {
		if taint.Key == key {
			return taint, true
		}
	}
	return corev1.Taint{}, false
}

func TestStaticOSTaintAlwaysOnNodeSpec(t *testing.T) {
	inv := Inventory{
		Host: Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30, Arch: "arm64"},
		Cfg:  config.Default(),
	}
	n := &corev1.Node{}
	if err := Apply(context.Background(), n, inv); err != nil {
		t.Fatal(err)
	}
	got, ok := taintByKey(n.Spec.Taints, wantOSKey)
	if !ok || got.Value != wantOSValue || got.Effect != corev1.TaintEffectNoSchedule {
		t.Fatalf("node spec taints = %+v, want %s=%s:NoSchedule", n.Spec.Taints, wantOSKey, wantOSValue)
	}
	if _, ok := taintByKey(n.Spec.Taints, types.TaintVMFullKey); ok {
		t.Fatalf("vm-full taint must stay unpublished: %+v", n.Spec.Taints)
	}
	if _, ok := taintByKey(n.Spec.Taints, types.TaintMacOSKey); !ok {
		t.Fatalf("existing macos taint missing: %+v", n.Spec.Taints)
	}
}

func TestStaticOSTaintStaysWhenOtherTaintsAreDisabled(t *testing.T) {
	cfg := config.Default()
	cfg.DisableTaint = true
	inv := Inventory{
		Host: Host{LogicalCPUs: 4, MemoryBytes: 8 << 30, DiskBytes: 100 << 30, Arch: "arm64"},
		Cfg:  cfg,
	}
	n := &corev1.Node{}
	if err := Apply(context.Background(), n, inv); err != nil {
		t.Fatal(err)
	}
	if _, ok := taintByKey(n.Spec.Taints, wantOSKey); !ok {
		t.Fatalf("DisableTaint removed %s: %+v", wantOSKey, n.Spec.Taints)
	}
	if _, ok := taintByKey(n.Spec.Taints, types.TaintProviderKey); ok {
		t.Fatalf("provider taint survived DisableTaint: %+v", n.Spec.Taints)
	}
	if _, ok := taintByKey(n.Spec.Taints, types.TaintMacOSKey); ok {
		t.Fatalf("darwin.node/macos survived DisableTaint: %+v", n.Spec.Taints)
	}
}

func TestStaticOSTaintSurvivesFullSlotTable(t *testing.T) {
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := slots.TryAcquire("a"); err != nil {
		t.Fatal(err)
	}
	if err := slots.TryAcquire("b"); err != nil {
		t.Fatal(err)
	}
	n := &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
		Key: types.TaintVMFullKey, Value: types.TaintVMFullValue, Effect: corev1.TaintEffectNoSchedule,
	}}}}
	inv := Inventory{
		Host:  Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30, Arch: "arm64"},
		Cfg:   config.Default(),
		Slots: slots,
	}
	if err := Apply(context.Background(), n, inv); err != nil {
		t.Fatal(err)
	}
	if _, ok := taintByKey(n.Spec.Taints, types.TaintVMFullKey); ok {
		t.Fatalf("stale vm-full taint survived a full slot table: %+v", n.Spec.Taints)
	}
	if _, ok := taintByKey(n.Spec.Taints, wantOSKey); !ok {
		t.Fatalf("full slot table dropped %s: %+v", wantOSKey, n.Spec.Taints)
	}
}
