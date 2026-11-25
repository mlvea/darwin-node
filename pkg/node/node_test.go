package node

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/types"

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
	// A full slot table must not publish darwin.node/vm-full. That taint
	// lands after kube-scheduler has already bound more pods. Capacity is
	// the extended resource darwin.node/vm-slot.
	if foundFull || !foundNAT {
		t.Fatalf("taints %+v", n.Spec.Taints)
	}
	slot := n.Status.Capacity[corev1.ResourceName("darwin.node/vm-slot")]
	allocSlot := n.Status.Allocatable[corev1.ResourceName("darwin.node/vm-slot")]
	if slot.Value() != 2 || allocSlot.Value() != 2 {
		t.Fatalf("vm-slot capacity %s allocatable %s, want 2 and 2", slot.String(), allocSlot.String())
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

func TestApplyDropsStaleVMFullTaint(t *testing.T) {
	n := &corev1.Node{Spec: corev1.NodeSpec{Taints: []corev1.Taint{{
		Key: types.TaintVMFullKey, Value: types.TaintVMFullValue, Effect: corev1.TaintEffectNoSchedule,
	}}}}
	inv := Inventory{
		Host: Host{LogicalCPUs: 4, MemoryBytes: 8 << 30, DiskBytes: 100 << 30, Arch: "arm64"},
		Cfg:  config.Default(),
	}
	if err := Apply(context.Background(), n, inv); err != nil {
		t.Fatal(err)
	}
	for _, taint := range n.Spec.Taints {
		if taint.Key == types.TaintVMFullKey {
			t.Fatalf("stale vm-full taint survived Apply: %+v", n.Spec.Taints)
		}
	}
}

func TestVMSlotCapacityFollowsConfiguredMax(t *testing.T) {
	slots, err := capacity.New(1)
	if err != nil {
		t.Fatal(err)
	}
	inv := Inventory{
		Host:  Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30},
		Cfg:   config.Config{MaxVMs: 1},
		Slots: slots,
	}
	capList, alloc, err := Capacity(inv)
	if err != nil {
		t.Fatal(err)
	}
	for _, list := range []corev1.ResourceList{capList, alloc} {
		q := list[corev1.ResourceName("darwin.node/vm-slot")]
		if q.Value() != 1 {
			t.Fatalf("vm-slot %s, want 1 (configured max)", q.String())
		}
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

func healthyInv() Inventory {
	return Inventory{
		Host: Host{LogicalCPUs: 4, MemoryBytes: 8 << 30, DiskBytes: 100 << 30, MemoryUsedFrac: 0.1, DiskUsedFrac: 0.1},
		Cfg:  config.Default(),
	}
}

func conditionStatus(inv Inventory, typ corev1.NodeConditionType) corev1.ConditionStatus {
	for _, c := range Conditions(inv) {
		if c.Type == typ {
			return c.Status
		}
	}
	return ""
}

func conditionMessage(inv Inventory, typ corev1.NodeConditionType) string {
	for _, c := range Conditions(inv) {
		if c.Type == typ {
			return c.Message
		}
	}
	return ""
}

// A failed host probe must not leave the node Ready with the last pressure
// bits. Virtual-kubelet refuses to patch status when Ping returns an error,
// so the failure is published as conditions and Ping itself stays nil.
func TestPingProbeFailureClearsReadyAndPressure(t *testing.T) {
	inv := healthyInv()
	p := NewStatusProvider(inv)
	p.Probe = func(context.Context) (Host, error) {
		return Host{}, errors.New("disk probe: disk unavailable")
	}
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping returned %v; virtual-kubelet would freeze the last Ready status", err)
	}
	got := p.Inventory()
	if st := conditionStatus(got, corev1.NodeReady); st != corev1.ConditionFalse {
		t.Fatalf("NodeReady %s, host probe failure left the node Ready", st)
	}
	if !strings.Contains(conditionMessage(got, corev1.NodeReady), "disk unavailable") {
		t.Fatalf("Ready message %q, want the probe error", conditionMessage(got, corev1.NodeReady))
	}
	for _, typ := range []corev1.NodeConditionType{corev1.NodeMemoryPressure, corev1.NodeDiskPressure} {
		if st := conditionStatus(got, typ); st != corev1.ConditionUnknown {
			t.Fatalf("%s %s, probe failure left a stale pressure condition", typ, st)
		}
	}
	if err := p.NotReady(); err == nil || !strings.Contains(err.Error(), "disk unavailable") {
		t.Fatalf("NotReady %v, pods would still be admitted", err)
	}
	// The last good measurement stays on the inventory; it is not published
	// while the probe is failed, and a zero host must not replace it.
	if got.Host.LogicalCPUs != inv.Host.LogicalCPUs {
		t.Fatalf("failed probe replaced the host: %+v", got.Host)
	}
}

func TestPingRecoveryRestoresReadyAndPressure(t *testing.T) {
	inv := healthyInv()
	p := NewStatusProvider(inv)
	failed := true
	p.Probe = func(context.Context) (Host, error) {
		if failed {
			return Host{}, errors.New("disk probe: disk unavailable")
		}
		h := inv.Host
		h.DiskUsedFrac = 0.95
		h.MemoryUsedFrac = 0.1
		return h, nil
	}
	if err := p.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if st := conditionStatus(p.Inventory(), corev1.NodeReady); st != corev1.ConditionFalse {
		t.Fatalf("NodeReady %s before recovery", st)
	}
	failed = false
	if err := p.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := p.Inventory()
	if st := conditionStatus(got, corev1.NodeReady); st != corev1.ConditionTrue {
		t.Fatalf("NodeReady %s, probe recovery left the node NotReady", st)
	}
	if st := conditionStatus(got, corev1.NodeDiskPressure); st != corev1.ConditionTrue {
		t.Fatalf("DiskPressure %s, recovery did not publish the new measurement", st)
	}
	if st := conditionStatus(got, corev1.NodeMemoryPressure); st != corev1.ConditionFalse {
		t.Fatalf("MemoryPressure %s, want False", st)
	}
	if err := p.NotReady(); err != nil {
		t.Fatalf("NotReady after recovery: %v", err)
	}
}

func TestPingRuntimeUnavailableClearsReady(t *testing.T) {
	p := NewStatusProvider(healthyInv())
	p.Probe = func(context.Context) (Host, error) {
		return healthyInv().Host, nil
	}
	p.Runtime = func(context.Context) error {
		return errors.New("Virtualization.framework is not available")
	}
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping returned %v; virtual-kubelet would freeze node status", err)
	}
	got := p.Inventory()
	if st := conditionStatus(got, corev1.NodeReady); st != corev1.ConditionFalse {
		t.Fatalf("NodeReady %s, runtime failure left the node Ready", st)
	}
	if st := conditionStatus(got, corev1.NodeConditionType("GuestRuntime")); st != corev1.ConditionFalse {
		t.Fatalf("GuestRuntime %s, want False", st)
	}
	if !strings.Contains(conditionMessage(got, corev1.NodeConditionType("GuestRuntime")), "not available") {
		t.Fatalf("GuestRuntime message %q", conditionMessage(got, corev1.NodeConditionType("GuestRuntime")))
	}
	// The host probe succeeded, so pressure is a real measurement.
	if st := conditionStatus(got, corev1.NodeDiskPressure); st != corev1.ConditionFalse {
		t.Fatalf("DiskPressure %s, a runtime failure must not hide a good disk probe", st)
	}
	if err := p.NotReady(); err == nil {
		t.Fatal("runtime failure still admitted pods")
	}
}

func TestPingCacheUnwritableClearsReady(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "cache")
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewStatusProvider(healthyInv())
	p.Probe = func(context.Context) (Host, error) {
		return healthyInv().Host, nil
	}
	p.CacheDir = dir
	if err := p.Ping(context.Background()); err != nil {
		t.Fatalf("Ping returned %v; virtual-kubelet would freeze node status", err)
	}
	got := p.Inventory()
	if st := conditionStatus(got, corev1.NodeReady); st != corev1.ConditionFalse {
		t.Fatalf("NodeReady %s, unwritable cache left the node Ready", st)
	}
	if !strings.Contains(conditionMessage(got, corev1.NodeReady), "cache") {
		t.Fatalf("Ready message %q, want the cache error", conditionMessage(got, corev1.NodeReady))
	}
	if st := conditionStatus(got, corev1.NodeMemoryPressure); st != corev1.ConditionFalse {
		t.Fatalf("MemoryPressure %s, cache failure must not mark pressure Unknown", st)
	}
	if err := p.NotReady(); err == nil {
		t.Fatal("unwritable cache still admitted pods")
	}
}

func TestProbeHostDiskErrorIsNotHealthy(t *testing.T) {
	orig := diskUsage
	diskUsage = func(context.Context, string) (uint64, uint64, error) {
		return 0, 0, errors.New("disk unavailable")
	}
	t.Cleanup(func() { diskUsage = orig })
	_, err := ProbeHost(context.Background())
	if err == nil || !strings.Contains(err.Error(), "disk") {
		t.Fatalf("err %v, disk failure was treated as a healthy host", err)
	}
}
