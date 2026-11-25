package sidecar

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/darwin-node/darwin-node/pkg/image"
	"github.com/docker/docker/api/types/registry"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func resolveEnv(pod *corev1.Pod, c corev1.Container, opts CreateOptions) ([]string, error) {
	if len(c.EnvFrom) > 0 {
		return nil, fmt.Errorf("sidecar %q: envFrom is not supported", c.Name)
	}
	if len(c.Env) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(c.Env))
	for _, ev := range c.Env {
		if ev.Name == "" || strings.ContainsAny(ev.Name, "=\x00") {
			return nil, fmt.Errorf("sidecar %q: env %q: invalid name", c.Name, ev.Name)
		}
		val := ev.Value
		if ev.ValueFrom != nil {
			var err error
			val, err = evalValueFrom(pod, c, ev, opts)
			if err != nil {
				return nil, fmt.Errorf("sidecar %q: env %s: %w", c.Name, ev.Name, err)
			}
		}
		if strings.ContainsRune(val, 0) {
			return nil, fmt.Errorf("sidecar %q: env %s: value contains NUL", c.Name, ev.Name)
		}
		out = append(out, ev.Name+"="+val)
	}
	return out, nil
}

func evalValueFrom(pod *corev1.Pod, c corev1.Container, ev corev1.EnvVar, opts CreateOptions) (string, error) {
	src := ev.ValueFrom
	switch {
	case src.ConfigMapKeyRef != nil:
		return configMapValue(opts, src.ConfigMapKeyRef)
	case src.SecretKeyRef != nil:
		return secretValue(opts, src.SecretKeyRef)
	case src.FieldRef != nil:
		if pod == nil {
			return "", fmt.Errorf("fieldRef requires a pod")
		}
		return fieldValue(pod, src.FieldRef.FieldPath)
	case src.ResourceFieldRef != nil:
		return resourceValue(pod, c.Name, src.ResourceFieldRef)
	default:
		return "", fmt.Errorf("unsupported valueFrom")
	}
}

func configMapValue(opts CreateOptions, ref *corev1.ConfigMapKeySelector) (string, error) {
	if ref.Name == "" || ref.Key == "" {
		return "", fmt.Errorf("configMapKeyRef name and key are required")
	}
	cm := opts.ConfigMaps[ref.Name]
	if cm == nil {
		if optional(ref.Optional) {
			return "", nil
		}
		return "", fmt.Errorf("configMap %q not found", ref.Name)
	}
	if v, ok := cm.Data[ref.Key]; ok {
		return v, nil
	}
	if b, ok := cm.BinaryData[ref.Key]; ok {
		return string(b), nil
	}
	if optional(ref.Optional) {
		return "", nil
	}
	return "", fmt.Errorf("configMap %q key %q not found", ref.Name, ref.Key)
}

func secretValue(opts CreateOptions, ref *corev1.SecretKeySelector) (string, error) {
	if ref.Name == "" || ref.Key == "" {
		return "", fmt.Errorf("secretKeyRef name and key are required")
	}
	sec := opts.Secrets[ref.Name]
	if sec == nil {
		if optional(ref.Optional) {
			return "", nil
		}
		return "", fmt.Errorf("secret %q not found", ref.Name)
	}
	if b, ok := sec.Data[ref.Key]; ok {
		return string(b), nil
	}
	if v, ok := sec.StringData[ref.Key]; ok {
		return v, nil
	}
	if optional(ref.Optional) {
		return "", nil
	}
	return "", fmt.Errorf("secret %q key %q not found", ref.Name, ref.Key)
}

func optional(p *bool) bool {
	return p != nil && *p
}

func fieldValue(pod *corev1.Pod, path string) (string, error) {
	switch path {
	case "metadata.name":
		return pod.Name, nil
	case "metadata.namespace":
		return pod.Namespace, nil
	case "metadata.uid":
		return string(pod.UID), nil
	case "spec.nodeName":
		return pod.Spec.NodeName, nil
	case "spec.serviceAccountName":
		return pod.Spec.ServiceAccountName, nil
	case "status.podIP":
		return pod.Status.PodIP, nil
	case "status.hostIP":
		return pod.Status.HostIP, nil
	default:
		if strings.HasPrefix(path, "metadata.labels") {
			return lookupQuoted(pod.Labels, path), nil
		}
		if strings.HasPrefix(path, "metadata.annotations") {
			return lookupQuoted(pod.Annotations, path), nil
		}
		return "", fmt.Errorf("unsupported fieldRef %q", path)
	}
}

func lookupQuoted(m map[string]string, expr string) string {
	start := strings.IndexByte(expr, '\'')
	if start < 0 {
		return ""
	}
	rest := expr[start+1:]
	end := strings.IndexByte(rest, '\'')
	if end < 0 {
		return ""
	}
	if m == nil {
		return ""
	}
	return m[rest[:end]]
}

func resourceValue(pod *corev1.Pod, fallback string, ref *corev1.ResourceFieldSelector) (string, error) {
	if ref == nil {
		return "", fmt.Errorf("nil resourceFieldRef")
	}
	name := ref.ContainerName
	if name == "" {
		name = fallback
	}
	c := findContainer(pod, name)
	if c == nil {
		return "", fmt.Errorf("container %q not found for resourceFieldRef", name)
	}
	switch ref.Resource {
	case "limits.cpu":
		return formatQuantity(c.Resources.Limits[corev1.ResourceCPU], ref.Divisor, true)
	case "requests.cpu":
		return formatQuantity(c.Resources.Requests[corev1.ResourceCPU], ref.Divisor, true)
	case "limits.memory":
		return formatQuantity(c.Resources.Limits[corev1.ResourceMemory], ref.Divisor, false)
	case "requests.memory":
		return formatQuantity(c.Resources.Requests[corev1.ResourceMemory], ref.Divisor, false)
	case "limits.ephemeral-storage":
		return formatQuantity(c.Resources.Limits[corev1.ResourceEphemeralStorage], ref.Divisor, false)
	case "requests.ephemeral-storage":
		return formatQuantity(c.Resources.Requests[corev1.ResourceEphemeralStorage], ref.Divisor, false)
	default:
		return "", fmt.Errorf("unsupported resourceFieldRef %q", ref.Resource)
	}
}

func findContainer(pod *corev1.Pod, name string) *corev1.Container {
	if pod == nil || name == "" {
		return nil
	}
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == name {
			return &pod.Spec.InitContainers[i]
		}
	}
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == name {
			return &pod.Spec.Containers[i]
		}
	}
	return nil
}

// formatQuantity matches kubelet: a zero divisor means 1, and the result
// is ceil(value/divisor) as a decimal integer. CPU is compared in
// milli-units so 500m / 1 is 1, not 0.
func formatQuantity(q, divisor resource.Quantity, cpu bool) (string, error) {
	if divisor.IsZero() {
		divisor = resource.MustParse("1")
	}
	var n float64
	if cpu {
		d := divisor.MilliValue()
		if d == 0 {
			return "", fmt.Errorf("cpu divisor is zero")
		}
		n = float64(q.MilliValue()) / float64(d)
	} else {
		d := divisor.Value()
		if d == 0 {
			return "", fmt.Errorf("divisor is zero")
		}
		n = float64(q.Value()) / float64(d)
	}
	return strconv.FormatInt(int64(math.Ceil(n)), 10), nil
}

func registryAuthHeader(ref string, creds image.RegistryCreds) (string, error) {
	got := creds.For(ref)
	if got.Empty() {
		return "", nil
	}
	return registry.EncodeAuthConfig(registry.AuthConfig{
		Username:      got.Username,
		Password:      got.Password,
		IdentityToken: got.IdentityToken,
		ServerAddress: got.Server,
	})
}
