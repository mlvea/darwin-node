package node

import (
	"context"
	"strings"
	"testing"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestCapacityReserved(t *testing.T) {
	slots, _ := capacity.New(2)
	inv := Inventory{
		Host: Host{LogicalCPUs: 10, MemoryBytes: 32 << 30, DiskBytes: 500 << 30, Arch: "arm64"},
		Cfg: config.Config{
			MaxVMs:            2,
			ReservedCPU:       resource.MustParse("2"),
			ReservedMemory:    resource.MustParse("4Gi"),
			ReservedEphemeral: resource.MustParse("20Gi"),
		},
		Slots: slots,
	}
	capList, alloc, err := Capacity(inv)
	if err != nil {
		t.Fatal(err)
	}
	pods := capList[corev1.ResourcePods]
	if pods.Value() != 2 {
		t.Fatalf("pods cap %s", pods.String())
	}
	vm := capList[corev1.ResourceName(types.ResourceVM)]
	if vm.Value() != 2 {
		t.Fatalf("vm cap %s", vm.String())
	}
	cpu := alloc[corev1.ResourceCPU]
	if cpu.Value() != 8 {
		t.Fatalf("alloc cpu %s", cpu.String())
	}
}

func TestApplyLabelsAndTaints(t *testing.T) {
	slots, _ := capacity.New(2)
	_ = slots.TryAcquire("a")
	_ = slots.TryAcquire("b")
	n := &corev1.Node{}
	inv := Inventory{
		Host: Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30, Arch: "arm64", CPUModel: "Apple M4 Pro", InternalIP: "10.0.0.5", HostID: "hw"},
		Cfg: config.Config{
			NodeName:          "mac-1",
			Runtime:           types.RuntimeFake,
			NetworkMode:       types.NetworkNAT,
			MaxVMs:            2,
			Graphics:          types.DefaultGraphics(),
			ListenPort:        10250,
			ReservedCPU:       resource.MustParse("2"),
			ReservedMemory:    resource.MustParse("4Gi"),
			ReservedEphemeral: resource.MustParse("20Gi"),
		},
		Slots: slots,
	}
	if err := Apply(context.Background(), n, inv); err != nil {
		t.Fatal(err)
	}
	if n.Labels[types.LabelGPUPassthrough] != "false" {
		t.Fatal("gpu passthrough must be false")
	}
	if n.Labels[types.LabelGPU] != "shared" {
		t.Fatal("gpu shared")
	}
	foundFull := false
	foundNAT := false
	for _, taint := range n.Spec.Taints {
		if taint.Key == types.TaintVMFullKey {
			foundFull = true
		}
		if taint.Key == types.TaintNATKey {
			foundNAT = true
		}
	}
	if !foundFull || !foundNAT {
		t.Fatalf("taints %+v", n.Spec.Taints)
	}
	var vmCond *corev1.NodeCondition
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == "VMCapacity" {
			vmCond = &n.Status.Conditions[i]
		}
	}
	if vmCond == nil || vmCond.Status != corev1.ConditionTrue {
		t.Fatalf("VMCapacity %+v", vmCond)
	}
	if !strings.HasPrefix(n.Status.NodeInfo.KubeletVersion, "v1.35.6") {
		t.Fatalf("kubelet version %s", n.Status.NodeInfo.KubeletVersion)
	}
}

func TestStatusProviderPingRefreshesDiskPressure(t *testing.T) {
	inv := Inventory{
		Host: Host{LogicalCPUs: 4, MemoryBytes: 8 << 30, DiskBytes: 100 << 30, DiskUsedFrac: 0.1},
		Cfg:  config.Default(),
	}
	p := NewStatusProvider(inv)
	p.Probe = func(context.Context) (Host, error) {
		h := inv.Host
		h.DiskUsedFrac = 0.95
		return h, nil
	}
	if err := p.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range Conditions(p.Inventory()) {
		if c.Type == corev1.NodeDiskPressure && c.Status == corev1.ConditionTrue {
			found = true
		}
	}
	if !found {
		t.Fatal("expected DiskPressure after ping")
	}
}

func TestMemoryPressure(t *testing.T) {
	inv := Inventory{
		Host: Host{LogicalCPUs: 4, MemoryBytes: 8 << 30, DiskBytes: 100 << 30, MemoryUsedFrac: 0.95, DiskUsedFrac: 0.1},
		Cfg:  config.Default(),
	}
	var mem *corev1.NodeCondition
	for i, c := range Conditions(inv) {
		if c.Type == corev1.NodeMemoryPressure {
			mem = &Conditions(inv)[i]
		}
	}
	_ = mem
	found := false
	for _, c := range Conditions(inv) {
		if c.Type == corev1.NodeMemoryPressure && c.Status == corev1.ConditionTrue {
			found = true
		}
	}
	if !found {
		t.Fatal("expected memory pressure")
	}
}
