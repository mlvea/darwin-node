package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// kubeClient is the cluster client when the credential resolver is the
// in-cluster one. Admission uses it only to write a pod Event.
func (p *Provider) kubeClient() kubernetes.Interface {
	if p == nil || p.creds == nil {
		return nil
	}
	switch c := p.creds.(type) {
	case KubeResolver:
		return c.Client
	case *KubeResolver:
		if c == nil {
			return nil
		}
		return c.Client
	default:
		return nil
	}
}

// emitK8sEvent records a Warning event on the pod. A missing client or
// an API error does not change the admission decision; the provider
// recorder already has the same reason.
func (p *Provider) emitK8sEvent(ctx context.Context, pod *corev1.Pod, reason, message string) {
	client := p.kubeClient()
	if client == nil || pod == nil {
		return
	}
	ns := pod.Namespace
	if ns == "" {
		ns = metav1.NamespaceDefault
	}
	now := time.Now()
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{
			Name:      eventName(pod.Name, now),
			Namespace: ns,
		},
		InvolvedObject: corev1.ObjectReference{
			Kind:       "Pod",
			Namespace:  ns,
			Name:       pod.Name,
			UID:        pod.UID,
			APIVersion: "v1",
		},
		Reason:              reason,
		Message:             message,
		Type:                corev1.EventTypeWarning,
		Source:              corev1.EventSource{Component: ComponentName, Host: p.cfg.NodeName},
		FirstTimestamp:      metav1.NewTime(now),
		LastTimestamp:       metav1.NewTime(now),
		EventTime:           metav1.NewMicroTime(now),
		Count:               1,
		ReportingController: ComponentName,
		ReportingInstance:   p.cfg.NodeName,
		Action:              reason,
	}
	parent := ctx
	if parent == nil {
		parent = context.Background()
	}
	ectx, cancel := context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)
	defer cancel()
	if _, err := client.CoreV1().Events(ns).Create(ectx, ev, metav1.CreateOptions{}); err != nil && p.events != nil {
		p.events.Warn(ctx, reason, fmt.Sprintf("kubernetes event: %v", err))
	}
}

func eventName(podName string, now time.Time) string {
	base := strings.Trim(podName, ".-")
	if base == "" {
		base = "pod"
	}
	name := fmt.Sprintf("%s.%x", base, now.UnixNano())
	if len(name) > 253 {
		name = strings.Trim(name[len(name)-253:], ".-")
	}
	if name == "" {
		return fmt.Sprintf("admission.%x", now.UnixNano())
	}
	return name
}
