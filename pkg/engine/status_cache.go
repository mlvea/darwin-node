package engine

import (
	"context"
	"time"

	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const sidecarListTimeout = 2 * time.Second

func sidecarContainerStatuses(containers []corev1.Container, cached []sidecar.Status) []corev1.ContainerStatus {
	byName := make(map[string]sidecar.Status, len(cached))
	for _, s := range cached {
		byName[s.Name] = s
	}
	out := make([]corev1.ContainerStatus, 0, len(containers))
	for _, c := range containers {
		s := byName[c.Name]
		run := s.State == "running"
		started := run
		st := corev1.ContainerStatus{Name: c.Name, Image: c.Image, Ready: run, Started: &started, ContainerID: s.ID}
		switch {
		case run:
			st.State.Running = &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(s.StartedAt)}
		case s.State == "terminated":
			st.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: int32(s.ExitCode), Message: s.Error}
		default:
			st.State.Waiting = &corev1.ContainerStateWaiting{Reason: "Waiting"}
		}
		out = append(out, st)
	}
	return out
}

// refreshSidecarStatus snapshots sidecar.List into rec without holding rec.mu during I/O.
func (e *Engine) refreshSidecarStatus(ctx context.Context, rec *podRecord) {
	rec.mu.Lock()
	pod := rec.pod
	rec.mu.Unlock()
	if pod == nil || len(pod.Spec.Containers) <= 1 {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, sidecarListTimeout)
	defer cancel()
	list, err := e.sidecar.List(lctx, pod.Namespace, pod.Name)
	if err != nil {
		return
	}
	rec.mu.Lock()
	rec.sidecarStatuses = list
	rec.mu.Unlock()
}
