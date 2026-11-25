package sidecar

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/container"
	corev1 "k8s.io/api/core/v1"
)

const (
	networkLabel    = "darwin-node"
	networkLabelPod = "pod"
)

// ValidatePodIsolation rejects host namespaces. They apply to every
// container in the pod, including a sidecar, and there is no safe way to
// honour them on the host Docker daemon.
func ValidatePodIsolation(pod *corev1.Pod) error {
	if pod == nil {
		return fmt.Errorf("nil pod")
	}
	if pod.Spec.HostNetwork {
		return fmt.Errorf("hostNetwork is not supported")
	}
	if pod.Spec.HostPID {
		return fmt.Errorf("hostPID is not supported")
	}
	if pod.Spec.HostIPC {
		return fmt.Errorf("hostIPC is not supported")
	}
	return nil
}

// ValidateContainerIsolation rejects a sidecar that would run with host
// privileges. The VM container is not passed here. runAsNonRoot requires an
// explicit non-zero runAsUser: the image USER is not inspected.
func ValidateContainerIsolation(pod *corev1.Pod, c corev1.Container) error {
	if err := ValidatePodIsolation(pod); err != nil {
		return err
	}
	sc := c.SecurityContext
	if sc != nil {
		if sc.Privileged != nil && *sc.Privileged {
			return fmt.Errorf("sidecar %q: privileged is not supported", c.Name)
		}
		if sc.AllowPrivilegeEscalation != nil && *sc.AllowPrivilegeEscalation {
			return fmt.Errorf("sidecar %q: allowPrivilegeEscalation is not supported", c.Name)
		}
		if sc.Capabilities != nil && len(sc.Capabilities.Add) > 0 {
			return fmt.Errorf("sidecar %q: adding capabilities is not supported", c.Name)
		}
		if sc.RunAsUser != nil && *sc.RunAsUser < 0 {
			return fmt.Errorf("sidecar %q: runAsUser must be >= 0", c.Name)
		}
		if sc.RunAsGroup != nil && *sc.RunAsGroup < 0 {
			return fmt.Errorf("sidecar %q: runAsGroup must be >= 0", c.Name)
		}
	}
	uid, _, nonRoot := mergedIdentity(pod, c)
	if nonRoot && (uid == nil || *uid == 0) {
		return fmt.Errorf("sidecar %q: runAsNonRoot requires an explicit non-zero runAsUser", c.Name)
	}
	for _, m := range c.VolumeMounts {
		if isDockerSock(m.MountPath) {
			return fmt.Errorf("sidecar %q: docker.sock mounts are not supported", c.Name)
		}
		src := volumeSource(pod, m.Name)
		if src != nil && src.HostPath != nil && isDockerSock(src.HostPath.Path) {
			return fmt.Errorf("sidecar %q: docker.sock mounts are not supported", c.Name)
		}
	}
	return nil
}

func isDockerSock(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	return strings.EqualFold(filepath.Base(filepath.Clean(path)), "docker.sock")
}

func mergedIdentity(pod *corev1.Pod, c corev1.Container) (uid, gid *int64, nonRoot bool) {
	if pod != nil && pod.Spec.SecurityContext != nil {
		psc := pod.Spec.SecurityContext
		uid, gid = psc.RunAsUser, psc.RunAsGroup
		if psc.RunAsNonRoot != nil {
			nonRoot = *psc.RunAsNonRoot
		}
	}
	if c.SecurityContext != nil {
		sc := c.SecurityContext
		if sc.RunAsUser != nil {
			uid = sc.RunAsUser
		}
		if sc.RunAsGroup != nil {
			gid = sc.RunAsGroup
		}
		if sc.RunAsNonRoot != nil {
			nonRoot = *sc.RunAsNonRoot
		}
	}
	return uid, gid, nonRoot
}

func userString(pod *corev1.Pod, c corev1.Container) string {
	uid, gid, _ := mergedIdentity(pod, c)
	switch {
	case uid != nil && gid != nil:
		return strconv.FormatInt(*uid, 10) + ":" + strconv.FormatInt(*gid, 10)
	case uid != nil:
		return strconv.FormatInt(*uid, 10)
	case gid != nil:
		return ":" + strconv.FormatInt(*gid, 10)
	default:
		return ""
	}
}

func readOnlyRoot(c corev1.Container) bool {
	return c.SecurityContext != nil && c.SecurityContext.ReadOnlyRootFilesystem != nil && *c.SecurityContext.ReadOnlyRootFilesystem
}

func droppedCaps(c corev1.Container) []corev1.Capability {
	if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil {
		return nil
	}
	return c.SecurityContext.Capabilities.Drop
}

func podNetworkName(pod *corev1.Pod) string {
	raw := ""
	if pod != nil {
		raw = string(pod.UID)
		if raw == "" {
			raw = pod.Namespace + "-" + pod.Name
		}
	}
	name := namePrefix + "-" + sanitizeNet(raw)
	const max = 200
	if len(name) > max {
		name = strings.TrimRight(name[:max], ".-_")
	}
	if name == "" || name == namePrefix {
		return namePrefix + "-pod"
	}
	return name
}

func sanitizeNet(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-_")
	if out == "" {
		return "pod"
	}
	return out
}

// dockerResources applies limits as the hard cap. A request is the cap
// only when no limit is set, which is what a sidecar without limits
// already received. CPUShares and MemoryReservation follow the request.
func dockerResources(c corev1.Container) (container.Resources, error) {
	var res container.Resources
	req := c.Resources.Requests
	lim := c.Resources.Limits
	if q, ok := lim[corev1.ResourceCPU]; ok && !q.IsZero() {
		if rq, ok := req[corev1.ResourceCPU]; ok && !rq.IsZero() && rq.Cmp(q) > 0 {
			return res, fmt.Errorf("sidecar %q: cpu request exceeds limit", c.Name)
		}
		res.NanoCPUs = q.MilliValue() * 1_000_000
	} else if q, ok := req[corev1.ResourceCPU]; ok && !q.IsZero() {
		res.NanoCPUs = q.MilliValue() * 1_000_000
	}
	if q, ok := req[corev1.ResourceCPU]; ok && !q.IsZero() {
		shares := q.MilliValue() * 1024 / 1000
		if shares < 2 {
			shares = 2
		}
		res.CPUShares = shares
	}
	if q, ok := lim[corev1.ResourceMemory]; ok && !q.IsZero() {
		if rq, ok := req[corev1.ResourceMemory]; ok && !rq.IsZero() && rq.Cmp(q) > 0 {
			return res, fmt.Errorf("sidecar %q: memory request exceeds limit", c.Name)
		}
		v, ok := q.AsInt64()
		if !ok {
			return res, fmt.Errorf("sidecar %q: memory limit is not an integer byte count", c.Name)
		}
		res.Memory = v
	} else if q, ok := req[corev1.ResourceMemory]; ok && !q.IsZero() {
		v, ok := q.AsInt64()
		if !ok {
			return res, fmt.Errorf("sidecar %q: memory request is not an integer byte count", c.Name)
		}
		res.Memory = v
	}
	if q, ok := req[corev1.ResourceMemory]; ok && !q.IsZero() {
		v, ok := q.AsInt64()
		if !ok {
			return res, fmt.Errorf("sidecar %q: memory request is not an integer byte count", c.Name)
		}
		res.MemoryReservation = v
	}
	return res, nil
}
