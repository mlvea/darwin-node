package engine

import (
	"maps"
	"sort"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
)

// UpdateMetadata stores a label or annotation change on a pod the engine
// already has. Virtual-kubelet calls UpdatePod for those, and for a
// container env slice whose order changed when it resolved the downward
// API again. A spec change is refused. The VM is left running either way,
// so a rejected update must not be mistaken for a failed pod by this process.
func (e *Engine) UpdateMetadata(pod *corev1.Pod) error {
	if pod == nil {
		return errdefs.InvalidInput("nil pod")
	}
	e.mu.RLock()
	rec := e.pods[Key(pod.Namespace, pod.Name)]
	e.mu.RUnlock()
	if rec == nil {
		return errdefs.NotFound("pod not found")
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.pod == nil {
		return errdefs.NotFound("pod not found")
	}
	if pod.UID != "" && rec.pod.UID != pod.UID {
		return errdefs.InvalidInput("pod updates are not supported; delete and recreate")
	}
	if !podSpecEqual(rec.pod.Spec, pod.Spec) {
		return errdefs.InvalidInput("pod updates are not supported; delete and recreate")
	}
	rec.pod.Labels = maps.Clone(pod.Labels)
	rec.pod.Annotations = maps.Clone(pod.Annotations)
	return nil
}

// podSpecEqual reports whether two specs are the same workload. Container
// env order is ignored: virtual-kubelet rebuilds that slice from a map on
// every sync, so a label change arrives with the variables swapped.
func podSpecEqual(a, b corev1.PodSpec) bool {
	aa := a.DeepCopy()
	bb := b.DeepCopy()
	normalizeSpecEnv(aa)
	normalizeSpecEnv(bb)
	return equality.Semantic.DeepEqual(aa, bb)
}

func normalizeSpecEnv(spec *corev1.PodSpec) {
	if spec == nil {
		return
	}
	for i := range spec.Containers {
		sortEnv(spec.Containers[i].Env)
	}
	for i := range spec.InitContainers {
		sortEnv(spec.InitContainers[i].Env)
	}
	for i := range spec.EphemeralContainers {
		sortEnv(spec.EphemeralContainers[i].Env)
	}
}

func sortEnv(env []corev1.EnvVar) {
	sort.Slice(env, func(i, j int) bool {
		if env[i].Name != env[j].Name {
			return env[i].Name < env[j].Name
		}
		return env[i].Value < env[j].Value
	})
}
