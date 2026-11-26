package node

import corev1 "k8s.io/api/core/v1"

const (
	// TaintOSKey is the static NoSchedule taint that keeps non-macOS pods
	// off this node. It is published on every node spec, including when
	// DisableTaint drops the provider and darwin.node/macos taints.
	TaintOSKey = "darwin.node/os"
	// TaintOSValue is the value of TaintOSKey.
	TaintOSValue = "macos"
)

// staticOSTaint is darwin.node/os=macos:NoSchedule.
func staticOSTaint() corev1.Taint {
	return corev1.Taint{
		Key:    TaintOSKey,
		Value:  TaintOSValue,
		Effect: corev1.TaintEffectNoSchedule,
	}
}
