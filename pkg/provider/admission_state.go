package provider

import (
	"sync"

	corev1 "k8s.io/api/core/v1"
)

// admissionLog stores pods rejected before a VM exists. GetPodStatus
// returns them as Failed so the virtual-kubelet status sync can publish
// that phase. They do not hold a slot.
type admissionLog struct {
	mu sync.Mutex
	by map[string]*rejectedPod
}

type rejectedPod struct {
	pod *corev1.Pod
	err *admissionError
}

func newAdmissionLog() *admissionLog {
	return &admissionLog{by: map[string]*rejectedPod{}}
}

func admissionKey(namespace, name string) string {
	return namespace + "/" + name
}

func (l *admissionLog) remember(pod *corev1.Pod, err *admissionError) {
	if l == nil || pod == nil || err == nil {
		return
	}
	stored := pod.DeepCopy()
	stored.Status = failedStatus(stored, err.Reason, err.Message)
	l.mu.Lock()
	defer l.mu.Unlock()
	l.by[admissionKey(pod.Namespace, pod.Name)] = &rejectedPod{pod: stored, err: err}
}

func (l *admissionLog) forget(namespace, name string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.by, admissionKey(namespace, name))
}

func (l *admissionLog) get(namespace, name string) (*corev1.Pod, *admissionError, bool) {
	if l == nil {
		return nil, nil, false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	got, ok := l.by[admissionKey(namespace, name)]
	if !ok || got == nil || got.pod == nil {
		return nil, nil, false
	}
	return got.pod.DeepCopy(), got.err, true
}

func (l *admissionLog) list() []*corev1.Pod {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]*corev1.Pod, 0, len(l.by))
	for _, got := range l.by {
		if got == nil || got.pod == nil {
			continue
		}
		out = append(out, got.pod.DeepCopy())
	}
	return out
}

func failedStatus(pod *corev1.Pod, reason, message string) corev1.PodStatus {
	st := corev1.PodStatus{
		Phase:   corev1.PodFailed,
		Reason:  reason,
		Message: message,
	}
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return st
	}
	c := pod.Spec.Containers[0]
	st.ContainerStatuses = []corev1.ContainerStatus{{
		Name:  c.Name,
		Image: c.Image,
		Ready: false,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1,
				Reason:   reason,
				Message:  message,
			},
		},
	}}
	return st
}
