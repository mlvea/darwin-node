package provider

import (
	"context"

	corev1 "k8s.io/api/core/v1"
)

// admit rejects a pod that is not a macOS guest or that does not fit in
// one VM. A rejection is stored as Failed, an event is recorded, and the
// error is returned. The engine is not called, so no slot is taken and
// no VM is booted. A pod the engine already has is left alone so a
// repeated create stays idempotent.
func (p *Provider) admit(ctx context.Context, pod *corev1.Pod) error {
	if p == nil {
		return &admissionError{Reason: ReasonUnsupportedPlatform, Message: "nil provider"}
	}
	if pod == nil {
		return &admissionError{Reason: ReasonUnsupportedPlatform, Message: "nil pod"}
	}
	if p.engineHas(pod.Namespace, pod.Name) {
		return nil
	}
	err := decideAdmission(pod, p.oneVMBudget())
	if err == nil {
		if p.rejected != nil {
			p.rejected.forget(pod.Namespace, pod.Name)
		}
		return nil
	}
	if p.rejected != nil {
		p.rejected.remember(pod, err)
	}
	if p.events != nil {
		p.events.Warn(ctx, err.Reason, err.Message)
	}
	p.emitK8sEvent(ctx, pod, err.Reason, err.Message)
	return err
}

func (p *Provider) engineHas(namespace, name string) bool {
	if p == nil || p.engine == nil {
		return false
	}
	_, err := p.engine.Get(namespace, name)
	return err == nil
}

func (p *Provider) rejectedPod(namespace, name string) (*corev1.Pod, bool) {
	if p == nil || p.rejected == nil {
		return nil, false
	}
	pod, _, ok := p.rejected.get(namespace, name)
	return pod, ok
}

func (p *Provider) rejectedErr(namespace, name string) (*admissionError, bool) {
	if p == nil || p.rejected == nil {
		return nil, false
	}
	_, err, ok := p.rejected.get(namespace, name)
	if !ok {
		return nil, false
	}
	return err, true
}

func (p *Provider) withRejected(pods []*corev1.Pod) []*corev1.Pod {
	if p == nil || p.rejected == nil {
		return pods
	}
	extra := p.rejected.list()
	if len(extra) == 0 {
		return pods
	}
	seen := make(map[string]struct{}, len(pods))
	for _, pod := range pods {
		if pod == nil {
			continue
		}
		seen[admissionKey(pod.Namespace, pod.Name)] = struct{}{}
	}
	for _, pod := range extra {
		if _, ok := seen[admissionKey(pod.Namespace, pod.Name)]; ok {
			continue
		}
		pods = append(pods, pod)
	}
	return pods
}
