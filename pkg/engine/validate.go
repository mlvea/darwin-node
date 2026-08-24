package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/darwin-node/darwin-node/pkg/volume"

	corev1 "k8s.io/api/core/v1"
)

const minMemory = 512 * 1024 * 1024 // 512 MiB

// ValidatePod rejects specs we cannot run. Fail closed.
// allowedHostPaths is the node-level hostPath prefix allowlist; empty denies all hostPath.
func ValidatePod(pod *corev1.Pod, allowedHostPaths []string) error {
	if pod == nil {
		return fmt.Errorf("nil pod")
	}
	if len(pod.Spec.Containers) == 0 {
		return fmt.Errorf("pod has no containers")
	}
	if pod.UID == "" {
		return fmt.Errorf("pod uid is required")
	}
	if pod.Spec.Containers[0].Image == "" {
		return fmt.Errorf("container[0] (macOS VM) has no image")
	}
	for _, ic := range pod.Spec.InitContainers {
		if LooksLikeVMImage(ic.Image) {
			return fmt.Errorf("init container %q cannot use a macOS VM image", ic.Name)
		}
	}
	for i := range pod.Spec.Volumes {
		v := pod.Spec.Volumes[i]
		if v.HostPath == nil {
			continue
		}
		if err := volume.AllowHostPath(v.HostPath.Path, allowedHostPaths); err != nil {
			return fmt.Errorf("volume %q: %w", v.Name, err)
		}
	}
	return nil
}

// LooksLikeVMImage reports whether ref is a baked darwin-node VM directory or disk.
func LooksLikeVMImage(image string) bool {
	if image == "" {
		return false
	}
	lower := strings.ToLower(image)
	if strings.HasSuffix(lower, ".img") || strings.HasSuffix(lower, ".ipsw") {
		return true
	}
	st, err := os.Stat(image)
	if err != nil || !st.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(image, "disk.img")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(image, "config.json")); err == nil {
		return true
	}
	return false
}

// VMResources extracts vCPU count and memory bytes from container[0].
// The VM is sized at the request. A limit below the request is rejected.
func VMResources(c corev1.Container) (cpu uint, memory uint64, err error) {
	req := c.Resources.Requests
	lim := c.Resources.Limits
	if req == nil {
		req = corev1.ResourceList{}
	}

	cpu = 2
	if q, ok := req[corev1.ResourceCPU]; ok && !q.IsZero() {
		cpu = uint((q.MilliValue() + 999) / 1000)
		if cpu < 1 {
			cpu = 1
		}
	}

	memory = 4 << 30
	if q, ok := req[corev1.ResourceMemory]; ok && !q.IsZero() {
		v, ok := q.AsInt64()
		if !ok || v <= 0 {
			return 0, 0, fmt.Errorf("invalid memory request")
		}
		memory = uint64(v)
	}
	if memory < minMemory {
		memory = minMemory
	}

	if lim != nil {
		if q, ok := lim[corev1.ResourceCPU]; ok && !q.IsZero() {
			lcpu := uint((q.MilliValue() + 999) / 1000)
			if lcpu < cpu {
				return 0, 0, fmt.Errorf("cpu limit %s is below request", q.String())
			}
		}
		if q, ok := lim[corev1.ResourceMemory]; ok && !q.IsZero() {
			v, ok := q.AsInt64()
			if !ok {
				return 0, 0, fmt.Errorf("invalid memory limit")
			}
			if uint64(v) < memory {
				return 0, 0, fmt.Errorf("memory limit %s is below request", q.String())
			}
		}
	}
	return cpu, memory, nil
}
