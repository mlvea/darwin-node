// Package node builds Kubernetes Node status: capacity, conditions, labels.
package node

import (
	"context"
	"fmt"
	"runtime"
	"strings"

	"github.com/mlvea/darwin-node/internal/labels"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// KubeletVersion matches the imported k8s.io/* module line (go.mod v0.35.6).
const KubeletVersion = "v1.35.6"

// Host is the measurable host. Tests inject a fake.
type Host struct {
	LogicalCPUs    int64
	MemoryBytes    int64
	DiskBytes      int64
	Hostname       string
	OSImage        string
	Kernel         string
	Arch           string
	CPUModel       string
	HostID         string
	InternalIP     string
	MemoryUsedFrac float64
	DiskUsedFrac   float64
}

// Inventory is a snapshot used to fill Node.status.
type Inventory struct {
	Host  Host
	Cfg   config.Config
	Slots *capacity.Slots
	// ProbeFailed is the latest host-probe error. Empty means the last
	// probe succeeded, including callers that never probe. While it is set,
	// memory and disk pressure are Unknown: the fractions are stale.
	ProbeFailed string
	// RuntimeFailed is set when the VM runtime cannot boot.
	RuntimeFailed string
	// CacheFailed is set when the cache directory is not writable.
	CacheFailed string
}

// Capacity returns capacity and allocatable resource lists.
func Capacity(inv Inventory) (capList, alloc corev1.ResourceList, err error) {
	h := inv.Host
	if h.LogicalCPUs <= 0 || h.MemoryBytes <= 0 {
		return nil, nil, fmt.Errorf("invalid host inventory")
	}
	maxVMs := types.AppleMaxConcurrentVMs
	if inv.Slots != nil {
		maxVMs = inv.Slots.Max()
	} else if inv.Cfg.MaxVMs > 0 {
		maxVMs = inv.Cfg.MaxVMs
	}

	cpuCap := *resource.NewQuantity(h.LogicalCPUs, resource.DecimalSI)
	memCap := *resource.NewQuantity(h.MemoryBytes, resource.BinarySI)
	diskCap := *resource.NewQuantity(h.DiskBytes, resource.BinarySI)
	pods := *resource.NewQuantity(int64(maxVMs), resource.DecimalSI)
	vms := *resource.NewQuantity(int64(maxVMs), resource.DecimalSI)
	metal := *resource.NewQuantity(int64(maxVMs), resource.DecimalSI)

	capList = corev1.ResourceList{
		corev1.ResourceCPU:                       cpuCap,
		corev1.ResourceMemory:                    memCap,
		corev1.ResourceEphemeralStorage:          diskCap,
		corev1.ResourcePods:                      pods,
		corev1.ResourceName(types.ResourceVM):    vms,
		corev1.ResourceName(types.ResourceMetal): metal,
	}

	resCPU := inv.Cfg.ReservedCPU.DeepCopy()
	resMem := inv.Cfg.ReservedMemory.DeepCopy()
	resDisk := inv.Cfg.ReservedEphemeral.DeepCopy()

	allocCPU := cpuCap.DeepCopy()
	allocCPU.Sub(resCPU)
	if allocCPU.Sign() < 0 {
		allocCPU = *resource.NewQuantity(0, resource.DecimalSI)
	}
	allocMem := memCap.DeepCopy()
	allocMem.Sub(resMem)
	if allocMem.Sign() < 0 {
		allocMem = *resource.NewQuantity(0, resource.BinarySI)
	}
	allocDisk := diskCap.DeepCopy()
	allocDisk.Sub(resDisk)
	if allocDisk.Sign() < 0 {
		allocDisk = *resource.NewQuantity(0, resource.BinarySI)
	}

	alloc = corev1.ResourceList{
		corev1.ResourceCPU:                       allocCPU,
		corev1.ResourceMemory:                    allocMem,
		corev1.ResourceEphemeralStorage:          allocDisk,
		corev1.ResourcePods:                      pods,
		corev1.ResourceName(types.ResourceVM):    vms,
		corev1.ResourceName(types.ResourceMetal): metal,
	}
	return capList, alloc, nil
}

// Apply mutates n with identity, capacity, labels, conditions, taints (spec).
func Apply(ctx context.Context, n *corev1.Node, inv Inventory) error {
	_ = ctx
	capList, alloc, err := Capacity(inv)
	if err != nil {
		return err
	}
	n.Status.Capacity = capList
	n.Status.Allocatable = alloc
	// Heartbeat moves on every publish. LastTransitionTime stays until the
	// condition status itself changes, including when this node object is
	// the one we published last time.
	prev := append([]corev1.NodeCondition(nil), n.Status.Conditions...)
	n.Status.Conditions = preserveTransitions(prev, Conditions(inv))
	if n.Labels == nil {
		n.Labels = map[string]string{}
	}
	for k, v := range Labels(inv) {
		n.Labels[k] = v
	}
	h := inv.Host
	n.Status.Addresses = []corev1.NodeAddress{
		{Type: corev1.NodeInternalIP, Address: h.InternalIP},
		{Type: corev1.NodeHostName, Address: inv.Cfg.NodeName},
	}
	n.Status.DaemonEndpoints = corev1.NodeDaemonEndpoints{
		KubeletEndpoint: corev1.DaemonEndpoint{Port: inv.Cfg.ListenPort},
	}
	n.Status.NodeInfo.MachineID = h.HostID
	n.Status.NodeInfo.KernelVersion = h.Kernel
	n.Status.NodeInfo.OSImage = h.OSImage
	n.Status.NodeInfo.ContainerRuntimeVersion = "vz://" + strings.ReplaceAll(h.OSImage, " ", "")
	n.Status.NodeInfo.OperatingSystem = "darwin"
	n.Status.NodeInfo.Architecture = h.Arch
	n.Status.NodeInfo.KubeletVersion = KubeletVersion + "-darwin-node-" + config.Version

	if n.Spec.Taints == nil {
		n.Spec.Taints = []corev1.Taint{}
	}
	n.Spec.Taints = mergeTaints(n.Spec.Taints, Taints(inv))
	return nil
}

// Labels for the node.
func Labels(inv Inventory) map[string]string {
	h := inv.Host
	arch := h.Arch
	if arch == "" {
		arch = runtime.GOARCH
	}
	m := map[string]string{
		corev1.LabelOSStable:             "darwin",
		corev1.LabelArchStable:           arch,
		corev1.LabelHostname:             inv.Cfg.NodeName,
		corev1.LabelNodeExcludeBalancers: "true",
		types.LabelRuntime:               string(inv.Cfg.Runtime),
		types.LabelNetworkMode:           string(inv.Cfg.NetworkMode),
		types.LabelGPU:                   "shared",
		types.LabelGPUModel:              "apple-paravirtual",
		types.LabelGPUMetal:              fmt.Sprintf("%t", inv.Cfg.Graphics.Enabled),
		types.LabelGPUPassthrough:        "false",
		types.LabelHostID:                h.HostID,
	}
	if h.CPUModel != "" {
		m[types.LabelCPUModel] = labels.SanitizeAppleCPUModel(h.CPUModel)
	}
	return m
}

// Taints that belong on the node spec.
func Taints(inv Inventory) []corev1.Taint {
	var out []corev1.Taint
	if !inv.Cfg.DisableTaint {
		out = append(out, corev1.Taint{
			Key: types.TaintProviderKey, Value: types.TaintProviderVal, Effect: corev1.TaintEffectNoSchedule,
		})
		out = append(out, corev1.Taint{
			Key: types.TaintMacOSKey, Value: types.TaintMacOSValue, Effect: corev1.TaintEffectNoSchedule,
		})
	}
	if inv.Cfg.NetworkMode == types.NetworkNAT && !inv.Cfg.AllowNATWorkloads {
		out = append(out, corev1.Taint{
			Key: types.TaintNATKey, Value: types.TaintNATValue, Effect: corev1.TaintEffectNoSchedule,
		})
	}
	if inv.Slots != nil && inv.Slots.Full() {
		out = append(out, corev1.Taint{
			Key: types.TaintVMFullKey, Value: types.TaintVMFullValue, Effect: corev1.TaintEffectNoSchedule,
		})
	}
	return out
}

func mergeTaints(existing, extra []corev1.Taint) []corev1.Taint {
	idx := map[string]corev1.Taint{}
	for _, t := range existing {
		idx[t.Key] = t
	}
	for _, t := range extra {
		idx[t.Key] = t
	}
	out := make([]corev1.Taint, 0, len(idx))
	for _, t := range idx {
		out = append(out, t)
	}
	return out
}

// Conditions builds node conditions from inventory. Memory/disk pressure
// use simple thresholds against reserved values (host probe can be richer).
func Conditions(inv Inventory) []corev1.NodeCondition {
	now := metav1.Now()
	ready := corev1.ConditionTrue
	readyReason, readyMsg := "KubeletReady", "darwin-node is ready"
	if msg := unreadyMessage(inv); msg != "" {
		ready = corev1.ConditionFalse
		readyReason = "KubeletNotReady"
		readyMsg = msg
	}
	vmFull := corev1.ConditionFalse
	vmReason := "VMSlotsAvailable"
	vmMsg := "macOS VM slots available"
	if inv.Slots != nil && inv.Slots.Full() {
		vmFull = corev1.ConditionTrue
		vmReason = "SlotsFull"
		vmMsg = fmt.Sprintf("all %d macOS VM slots in use", inv.Slots.Max())
	}
	metal := corev1.ConditionTrue
	metalReason := "MetalConfigured"
	if !inv.Cfg.Graphics.Enabled {
		metal = corev1.ConditionFalse
		metalReason = "GraphicsDisabled"
	}
	memPress := corev1.ConditionFalse
	memReason, memMsg := "HasSufficientMemory", "host memory above reservation"
	diskPress := corev1.ConditionFalse
	diskReason, diskMsg := "HasNoDiskPressure", "host disk above reservation"
	if inv.ProbeFailed != "" {
		memPress = corev1.ConditionUnknown
		memReason, memMsg = "ProbeFailed", inv.ProbeFailed
		diskPress = corev1.ConditionUnknown
		diskReason, diskMsg = "ProbeFailed", inv.ProbeFailed
	} else {
		if inv.Host.MemoryUsedFrac > 0.90 {
			memPress = corev1.ConditionTrue
			memReason, memMsg = "MemoryPressure", "host memory used > 90%"
		}
		if inv.Host.DiskUsedFrac > 0.90 {
			diskPress = corev1.ConditionTrue
			diskReason, diskMsg = "DiskPressure", "host disk used > 90%"
		}
	}
	guest := corev1.ConditionTrue
	guestReason, guestMsg := "RuntimeReady", string(inv.Cfg.Runtime)+" runtime selected"
	if inv.RuntimeFailed != "" {
		guest = corev1.ConditionFalse
		guestReason = "RuntimeUnavailable"
		guestMsg = inv.RuntimeFailed
	}

	return []corev1.NodeCondition{
		cond(corev1.NodeReady, ready, now, readyReason, readyMsg),
		cond(corev1.NodeMemoryPressure, memPress, now, memReason, memMsg),
		cond(corev1.NodeDiskPressure, diskPress, now, diskReason, diskMsg),
		cond(corev1.NodePIDPressure, corev1.ConditionFalse, now, "HasNoPIDPressure", "pid pressure not applicable"),
		cond(corev1.NodeNetworkUnavailable, corev1.ConditionFalse, now, "RouteCreated", "host network configured"),
		cond(corev1.NodeConditionType("VMCapacity"), vmFull, now, vmReason, vmMsg),
		cond(corev1.NodeConditionType("MetalReady"), metal, now, metalReason, "paravirtualized Metal device"),
		cond(corev1.NodeConditionType("GuestRuntime"), guest, now, guestReason, guestMsg),
	}
}

// preserveTransitions copies LastTransitionTime from prev when the status
// did not change. Reason and message may change without moving it.
// LastHeartbeatTime is left as Conditions set it.
func preserveTransitions(prev, next []corev1.NodeCondition) []corev1.NodeCondition {
	if len(prev) == 0 || len(next) == 0 {
		return next
	}
	byType := make(map[corev1.NodeConditionType]corev1.NodeCondition, len(prev))
	for _, c := range prev {
		byType[c.Type] = c
	}
	for i := range next {
		old, ok := byType[next[i].Type]
		if !ok || old.Status != next[i].Status || old.LastTransitionTime.IsZero() {
			continue
		}
		next[i].LastTransitionTime = old.LastTransitionTime
	}
	return next
}

func cond(t corev1.NodeConditionType, st corev1.ConditionStatus, now metav1.Time, reason, msg string) corev1.NodeCondition {
	return corev1.NodeCondition{
		Type:               t,
		Status:             st,
		LastHeartbeatTime:  now,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            msg,
	}
}
