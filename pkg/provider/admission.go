package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mlvea/darwin-node/pkg/engine"
	"github.com/mlvea/darwin-node/pkg/node"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// ReasonUnsupportedPlatform is the event and pod-status reason for a
	// pod this node will not run. The guest is macOS only.
	ReasonUnsupportedPlatform = "UnsupportedPlatform"
	// ReasonVMBudgetExceeded is the event and pod-status reason when the
	// pod's effective CPU or memory request does not fit in one VM.
	ReasonVMBudgetExceeded = "VMBudgetExceeded"
	// AnnotationDiskImage opts container 0 in as a darwin-node macOS disk
	// image when its repository name is not one of the conventional names.
	// The value must be "true". A host path still has to be a baked image.
	AnnotationDiskImage = "darwin.node/disk-image"
)

// admissionError is a terminal CreatePod rejection. The pod is stored
// Failed and no VM is started.
type admissionError struct {
	Reason  string
	Message string
}

func (e *admissionError) Error() string {
	if e == nil {
		return ""
	}
	return e.Reason + ": " + e.Message
}

// osLabelKeys are the node labels that name the operating system.
var osLabelKeys = map[string]struct{}{
	corev1.LabelOSStable:    {},
	"beta.kubernetes.io/os": {},
}

// vmBudget is the CPU and memory one VM can be given: the node's
// allocatable (host minus the configured reservation).
type vmBudget struct {
	CPU    resource.Quantity
	Memory resource.Quantity
	Known  bool
}

func (p *Provider) oneVMBudget() vmBudget {
	if p == nil {
		return vmBudget{}
	}
	inv := p.inv
	inv.Cfg = p.cfg
	_, alloc, err := node.Capacity(inv)
	if err != nil {
		return vmBudget{}
	}
	return vmBudget{
		CPU:    alloc[corev1.ResourceCPU],
		Memory: alloc[corev1.ResourceMemory],
		Known:  true,
	}
}

// decideAdmission reports why pod cannot run here. Nil means it is a
// macOS workload whose effective request fits in one VM.
func decideAdmission(pod *corev1.Pod, b vmBudget) *admissionError {
	if pod == nil {
		return &admissionError{Reason: ReasonUnsupportedPlatform, Message: "nil pod"}
	}
	if msg := osProblem(pod); msg != "" {
		return &admissionError{Reason: ReasonUnsupportedPlatform, Message: msg}
	}
	if msg := vmImageProblem(pod); msg != "" {
		return &admissionError{Reason: ReasonUnsupportedPlatform, Message: msg}
	}
	eff := effectiveResources(pod)
	if !b.Known {
		if eff.CPU.Sign() > 0 || eff.Memory.Sign() > 0 {
			return &admissionError{
				Reason:  ReasonVMBudgetExceeded,
				Message: "node inventory cannot size one VM for this request",
			}
		}
		return nil
	}
	overCPU := eff.CPU.Cmp(b.CPU) > 0
	overMem := eff.Memory.Cmp(b.Memory) > 0
	if !overCPU && !overMem {
		return nil
	}
	return &admissionError{
		Reason: ReasonVMBudgetExceeded,
		Message: fmt.Sprintf(
			"effective request cpu=%s memory=%s exceeds what one VM can be given (cpu=%s memory=%s, host allocatable); init containers and sidecars share that one VM and one %s",
			eff.CPU.String(), eff.Memory.String(), b.CPU.String(), b.Memory.String(), "darwin.node/vm-slot",
		),
	}
}

func osProblem(pod *corev1.Pod) string {
	if pod.Spec.OS != nil {
		name := strings.TrimSpace(string(pod.Spec.OS.Name))
		if name != "" && name != "darwin" {
			return fmt.Sprintf("spec.os.name=%s; this node runs macOS guests only", name)
		}
	}
	for key, value := range pod.Spec.NodeSelector {
		if _, ok := osLabelKeys[key]; !ok {
			continue
		}
		if value != "darwin" {
			return fmt.Sprintf("nodeSelector %s=%s; this node runs macOS guests only", key, value)
		}
	}
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		return ""
	}
	na := pod.Spec.Affinity.NodeAffinity
	if req := na.RequiredDuringSchedulingIgnoredDuringExecution; req != nil {
		for _, term := range req.NodeSelectorTerms {
			if msg := termOS(term); msg != "" {
				return msg
			}
		}
	}
	for _, pref := range na.PreferredDuringSchedulingIgnoredDuringExecution {
		if msg := termOS(pref.Preference); msg != "" {
			return msg
		}
	}
	return ""
}

func termOS(term corev1.NodeSelectorTerm) string {
	reqs := make([]corev1.NodeSelectorRequirement, 0, len(term.MatchExpressions)+len(term.MatchFields))
	reqs = append(reqs, term.MatchExpressions...)
	reqs = append(reqs, term.MatchFields...)
	for _, req := range reqs {
		if _, ok := osLabelKeys[req.Key]; !ok {
			continue
		}
		if msg := requirementOS(req); msg != "" {
			return msg
		}
	}
	return ""
}

func requirementOS(req corev1.NodeSelectorRequirement) string {
	switch req.Operator {
	case corev1.NodeSelectorOpIn, "":
		if containsValue(req.Values, "linux") {
			return fmt.Sprintf("node affinity %s In %v selects linux; this node runs macOS guests only", req.Key, req.Values)
		}
		if !containsValue(req.Values, "darwin") {
			return fmt.Sprintf("node affinity %s In %v does not match kubernetes.io/os=darwin", req.Key, req.Values)
		}
	case corev1.NodeSelectorOpNotIn:
		if containsValue(req.Values, "darwin") {
			return fmt.Sprintf("node affinity %s NotIn %v excludes kubernetes.io/os=darwin", req.Key, req.Values)
		}
	case corev1.NodeSelectorOpExists:
		return ""
	case corev1.NodeSelectorOpDoesNotExist:
		return fmt.Sprintf("node affinity %s DoesNotExist excludes this node (kubernetes.io/os=darwin)", req.Key)
	default:
		return fmt.Sprintf("node affinity %s %s %v does not match kubernetes.io/os=darwin", req.Key, req.Operator, req.Values)
	}
	return ""
}

func containsValue(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// vmImageProblem checks container 0, the macOS VM. Sidecar and init
// images run inside that guest and are not disk images.
//
// A host path must pass engine.LooksLikeVMImage (.img, .ipsw, or a
// directory with disk.img or config.json). A registry reference must
// parse with distribution/reference, the same parser pkg/image uses,
// and the repository's last component must be macos, macosvm, macosvz,
// or darwin-node, or start with macos-, macosvm-, macosvz-, or
// darwin-node-. Docker Hub library images (docker.io/library/...) are
// ordinary Linux containers and are rejected unless the pod sets
// darwin.node/disk-image=true.
func vmImageProblem(pod *corev1.Pod) string {
	if len(pod.Spec.Containers) == 0 {
		return "pod has no containers; container 0 is the macOS VM"
	}
	c := pod.Spec.Containers[0]
	ref := strings.TrimSpace(c.Image)
	if ref == "" {
		return fmt.Sprintf("container %q has no image; container 0 must be a darwin-node macOS disk image", c.Name)
	}
	if engine.LooksLikeVMImage(ref) {
		return ""
	}
	if hostImageRef(ref) {
		return fmt.Sprintf("container %q image %q is a host path but not a darwin-node macOS disk image (want a directory with disk.img or config.json, or a .img/.ipsw file)", c.Name, ref)
	}
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		return fmt.Sprintf("container %q image %q is not a darwin-node macOS disk image reference: %v", c.Name, ref, err)
	}
	if diskImageAnnotation(pod) {
		return ""
	}
	if reference.Domain(named) == "docker.io" && strings.HasPrefix(reference.Path(named), "library/") {
		return fmt.Sprintf("container %q image %q is a Docker Hub library image, not a darwin-node macOS disk image", c.Name, ref)
	}
	if !isDiskImageLeaf(repositoryLeaf(reference.Path(named))) {
		return fmt.Sprintf("container %q image %q is not a darwin-node macOS disk image (repository name must be macos, macosvm, macosvz, or darwin-node, or set %s=true)", c.Name, ref, AnnotationDiskImage)
	}
	return ""
}

func diskImageAnnotation(pod *corev1.Pod) bool {
	if pod == nil || pod.Annotations == nil {
		return false
	}
	return pod.Annotations[AnnotationDiskImage] == "true"
}

func repositoryLeaf(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}

func isDiskImageLeaf(leaf string) bool {
	switch leaf {
	case "macos", "macosvm", "macosvz", "darwin-node":
		return true
	}
	for _, pre := range []string{"macos-", "macosvm-", "macosvz-", "darwin-node-"} {
		if strings.HasPrefix(leaf, pre) && len(leaf) > len(pre) {
			return true
		}
	}
	return false
}

// hostImageRef matches engine.hostImagePath. Registry names are not
// host paths unless a directory with that name exists.
func hostImageRef(ref string) bool {
	if ref == "" {
		return false
	}
	if filepath.IsAbs(ref) || strings.HasPrefix(ref, ".") {
		return true
	}
	fi, err := os.Lstat(ref)
	if err != nil {
		return false
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return true
	}
	return fi.IsDir()
}

// effectiveResources is the kubelet pod request for CPU and memory.
// Restartable init containers (restartPolicy Always) are added to the
// app-container sum. Other init containers contribute only their own
// request, and a later non-restartable init also counts restartable
// inits that the API ordered ahead of it. The pod request is the max
// of that init total and the app total. Pod overhead is added after.
// Extended resources, including darwin.node/vm-slot, are not included:
// one pod takes one slot regardless of how many containers request it.
func effectiveResources(pod *corev1.Pod) vmRequest {
	var cpu, mem resource.Quantity
	if pod == nil {
		return vmRequest{}
	}
	for _, c := range pod.Spec.Containers {
		cpu = addQty(cpu, resourceQty(c.Resources.Requests, corev1.ResourceCPU))
		mem = addQty(mem, resourceQty(c.Resources.Requests, corev1.ResourceMemory))
	}
	var restartCPU, restartMem resource.Quantity
	var initCPU, initMem resource.Quantity
	for _, c := range pod.Spec.InitContainers {
		cCPU := resourceQty(c.Resources.Requests, corev1.ResourceCPU)
		cMem := resourceQty(c.Resources.Requests, corev1.ResourceMemory)
		if restartableInit(c) {
			cpu = addQty(cpu, cCPU)
			mem = addQty(mem, cMem)
			restartCPU = addQty(restartCPU, cCPU)
			restartMem = addQty(restartMem, cMem)
			cCPU, cMem = restartCPU, restartMem
		} else {
			cCPU = addQty(cCPU, restartCPU)
			cMem = addQty(cMem, restartMem)
		}
		initCPU = maxQty(initCPU, cCPU)
		initMem = maxQty(initMem, cMem)
	}
	cpu = maxQty(cpu, initCPU)
	mem = maxQty(mem, initMem)
	cpu = addQty(cpu, resourceQty(pod.Spec.Overhead, corev1.ResourceCPU))
	mem = addQty(mem, resourceQty(pod.Spec.Overhead, corev1.ResourceMemory))
	return vmRequest{CPU: cpu, Memory: mem}
}

type vmRequest struct {
	CPU    resource.Quantity
	Memory resource.Quantity
}

func restartableInit(c corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

func resourceQty(list corev1.ResourceList, name corev1.ResourceName) resource.Quantity {
	if list == nil {
		return resource.Quantity{}
	}
	q, ok := list[name]
	if !ok {
		return resource.Quantity{}
	}
	return q.DeepCopy()
}

func addQty(a, b resource.Quantity) resource.Quantity {
	out := a.DeepCopy()
	out.Add(b)
	return out
}

func maxQty(a, b resource.Quantity) resource.Quantity {
	if a.Cmp(b) >= 0 {
		return a.DeepCopy()
	}
	return b.DeepCopy()
}
