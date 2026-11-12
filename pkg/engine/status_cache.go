package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const sidecarListTimeout = 2 * time.Second

func sidecarContainerStatuses(containers []corev1.Container, cached []sidecar.Status, restarts map[string]int32, last map[string]*corev1.ContainerStateTerminated) []corev1.ContainerStatus {
	byName := make(map[string]sidecar.Status, len(cached))
	for _, s := range cached {
		byName[s.Name] = s
	}
	out := make([]corev1.ContainerStatus, 0, len(containers))
	for _, c := range containers {
		s := byName[c.Name]
		run := s.State == "running"
		started := sidecarStarted(s)
		st := corev1.ContainerStatus{Name: c.Name, Image: c.Image, Ready: run, Started: &started, ContainerID: s.ID, RestartCount: restarts[c.Name]}
		if prev := last[c.Name]; prev != nil {
			cp := *prev
			st.LastTerminationState.Terminated = &cp
		}
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

// sidecarStarted reports whether this sidecar has entered running.
// Ready stays "running now". A later exit does not clear Started.
// Sidecar startup probes are not executed.
func sidecarStarted(s sidecar.Status) bool {
	if s.State == "running" {
		return true
	}
	return s.State == "terminated" && !s.StartedAt.IsZero()
}

// sidecarsRunningLocked reports whether every hybrid sidecar is running.
// The caller holds rec.mu. A pod with no sidecars is running.
func sidecarsRunningLocked(rec *podRecord) bool {
	if rec.pod == nil || len(rec.pod.Spec.Containers) < 2 {
		return true
	}
	byName := make(map[string]sidecar.Status, len(rec.sidecarStatuses))
	for _, st := range rec.sidecarStatuses {
		byName[st.Name] = st
	}
	for _, c := range rec.pod.Spec.Containers[1:] {
		st, ok := byName[c.Name]
		if !ok || st.State != "running" {
			return false
		}
	}
	return true
}

func sidecarDownMessageLocked(rec *podRecord) string {
	byName := make(map[string]sidecar.Status, len(rec.sidecarStatuses))
	for _, st := range rec.sidecarStatuses {
		byName[st.Name] = st
	}
	for _, c := range rec.pod.Spec.Containers[1:] {
		st, ok := byName[c.Name]
		if ok && st.State == "running" {
			continue
		}
		if ok && st.State == "terminated" {
			return fmt.Sprintf("sidecar %s exited %d", c.Name, st.ExitCode)
		}
		if !ok {
			return fmt.Sprintf("sidecar %s has no status", c.Name)
		}
		return fmt.Sprintf("sidecar %s is %s", c.Name, st.State)
	}
	return "sidecar not running"
}

// sidecarExitFailsPod reports whether this exit is terminal. Always restarts
// on any exit. OnFailure restarts a non-zero exit. Never does not, and a
// non-zero exit fails the pod. A zero exit completed successfully.
func sidecarExitFailsPod(policy corev1.RestartPolicy, exitCode int) bool {
	return exitCode != 0 && policy == corev1.RestartPolicyNever
}

// sidecarShouldRestart reports whether a terminated sidecar is started again.
// An empty policy is Always, which is the Kubernetes default.
func sidecarShouldRestart(policy corev1.RestartPolicy, exitCode int) bool {
	switch policy {
	case "", corev1.RestartPolicyAlways:
		return true
	case corev1.RestartPolicyOnFailure:
		return exitCode != 0
	default:
		return false
	}
}

// noteSidecarExit fails the pod when a hybrid sidecar has exited and the
// restart policy will not bring it back. False means the caller must stop.
func (e *Engine) noteSidecarExit(rec *podRecord) bool {
	rec.mu.Lock()
	pod := rec.pod
	phase := rec.phase
	statuses := append([]sidecar.Status(nil), rec.sidecarStatuses...)
	rec.mu.Unlock()
	if pod == nil || len(pod.Spec.Containers) < 2 {
		return true
	}
	if phase != corev1.PodPending && phase != corev1.PodRunning {
		return true
	}
	byName := make(map[string]sidecar.Status, len(statuses))
	for _, st := range statuses {
		byName[st.Name] = st
	}
	for _, c := range pod.Spec.Containers[1:] {
		st, ok := byName[c.Name]
		if !ok || st.State != "terminated" || !sidecarExitFailsPod(pod.Spec.RestartPolicy, st.ExitCode) {
			continue
		}
		err := fmt.Errorf("sidecar %s exited %d", c.Name, st.ExitCode)
		if st.Error != "" {
			err = fmt.Errorf("sidecar %s exited %d: %s", c.Name, st.ExitCode, st.Error)
		}
		e.fail(rec, err)
		return false
	}
	return true
}

// restartExitedSidecars starts sidecars whose policy says to try again.
// Create is the runtime's restart: Docker starts the existing container, and
// the fake runtime replaces that container's status. A Create error fails
// the pod and releases the VM slot. The caller must stop when this is false.
// Restarts run on the probe interval. There is no CrashLoopBackOff.
func (e *Engine) restartExitedSidecars(ctx context.Context, rec *podRecord) bool {
	rec.mu.Lock()
	if rec.pod == nil || len(rec.pod.Spec.Containers) < 2 {
		rec.mu.Unlock()
		return true
	}
	if rec.phase != corev1.PodPending && rec.phase != corev1.PodRunning {
		rec.mu.Unlock()
		return true
	}
	pod := rec.pod.DeepCopy()
	volRoot := rec.volRoot
	statuses := append([]sidecar.Status(nil), rec.sidecarStatuses...)
	rec.mu.Unlock()

	byName := make(map[string]sidecar.Status, len(statuses))
	for _, st := range statuses {
		byName[st.Name] = st
	}
	restarted := false
	for _, c := range pod.Spec.Containers[1:] {
		st, ok := byName[c.Name]
		if !ok || st.State != "terminated" || !sidecarShouldRestart(pod.Spec.RestartPolicy, st.ExitCode) {
			continue
		}
		if err := e.sidecar.Create(ctx, pod, c, volRoot); err != nil {
			rec.mu.Lock()
			phase := rec.phase
			rec.mu.Unlock()
			if (phase != corev1.PodRunning && phase != corev1.PodPending) || ctx.Err() != nil {
				return false
			}
			e.fail(rec, fmt.Errorf("sidecar %s restart: %w", c.Name, err))
			return false
		}
		reason := "Error"
		if st.ExitCode == 0 {
			reason = "Completed"
		}
		term := &corev1.ContainerStateTerminated{ExitCode: int32(st.ExitCode), Reason: reason, Message: st.Error}
		if !st.FinishedAt.IsZero() {
			term.FinishedAt = metav1.NewTime(st.FinishedAt)
		}
		rec.mu.Lock()
		if rec.sidecarRestarts == nil {
			rec.sidecarRestarts = map[string]int32{}
		}
		if rec.sidecarLastExit == nil {
			rec.sidecarLastExit = map[string]*corev1.ContainerStateTerminated{}
		}
		rec.sidecarRestarts[c.Name]++
		rec.sidecarLastExit[c.Name] = term
		rec.mu.Unlock()
		e.events.Normal(ctx, event.ReasonStarted, fmt.Sprintf("sidecar %s restarted", c.Name))
		restarted = true
	}
	if restarted {
		e.refreshSidecarStatus(ctx, rec)
	}
	return true
}
