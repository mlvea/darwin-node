package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/volume"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (e *Engine) runInitContainers(ctx context.Context, rec *podRecord, pod *corev1.Pod, creds Credentials, volRoot string) error {
	if len(pod.Spec.InitContainers) == 0 {
		rec.mu.Lock()
		rec.initialized = true
		rec.mu.Unlock()
		return nil
	}
	for _, ic := range pod.Spec.InitContainers {
		if LooksLikeVMImage(ic.Image) {
			return fmt.Errorf("init container %q: macOS VM images cannot run as init containers", ic.Name)
		}
		if _, _, err := volume.Materialize(creds.volumeReq(pod, ic, volRoot, e.cfg.AllowedHostPaths)); err != nil {
			return err
		}
		e.events.Normal(ctx, event.ReasonInitStarted, "init container "+ic.Name)
		if err := e.sidecar.Create(sidecar.WithCreateOptions(ctx, e.sidecarCreateOptions(creds)), pod, ic, volRoot); err != nil {
			return err
		}
		st, err := e.waitSidecarExit(ctx, pod.Namespace, pod.Name, ic.Name)
		e.noteInitStatus(rec, ic, st)
		if err != nil {
			return err
		}
		e.events.Normal(ctx, event.ReasonInitExited, "init container "+ic.Name+" completed")
	}
	rec.mu.Lock()
	rec.initialized = true
	rec.mu.Unlock()
	return nil
}

func initContainerStatus(ic corev1.Container, st sidecar.Status) corev1.ContainerStatus {
	started := st.State == "running" || st.State == "terminated"
	cs := corev1.ContainerStatus{
		Name:        ic.Name,
		Image:       ic.Image,
		Ready:       st.State == "terminated" && st.ExitCode == 0,
		Started:     &started,
		ContainerID: st.ID,
	}
	switch st.State {
	case "terminated":
		reason := "Completed"
		if st.ExitCode != 0 {
			reason = "Error"
		}
		term := &corev1.ContainerStateTerminated{ExitCode: int32(st.ExitCode), Reason: reason, Message: st.Error}
		if !st.StartedAt.IsZero() {
			term.StartedAt = metav1.NewTime(st.StartedAt)
		}
		if !st.FinishedAt.IsZero() {
			term.FinishedAt = metav1.NewTime(st.FinishedAt)
		}
		cs.State.Terminated = term
	case "running":
		run := &corev1.ContainerStateRunning{}
		if !st.StartedAt.IsZero() {
			run.StartedAt = metav1.NewTime(st.StartedAt)
		}
		cs.State.Running = run
	default:
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: "PodInitializing", Message: st.Error}
	}
	return cs
}

func (e *Engine) noteInitStatus(rec *podRecord, ic corev1.Container, st sidecar.Status) {
	if st.State == "" && st.Name == "" && st.ID == "" {
		return
	}
	cs := initContainerStatus(ic, st)
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for i := range rec.initStatuses {
		if rec.initStatuses[i].Name == ic.Name {
			rec.initStatuses[i] = cs
			return
		}
	}
	rec.initStatuses = append(rec.initStatuses, cs)
}

func (e *Engine) waitSidecarExit(ctx context.Context, ns, name, container string) (sidecar.Status, error) {
	limit := e.cfg.InitTimeout
	if limit <= 0 {
		limit = 2 * time.Minute
	}
	deadline := time.Now().Add(limit)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	backoff := 50 * time.Millisecond
	const maxBackoff = time.Second
	consecutiveErr := 0
	var last sidecar.Status
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		st, err := e.sidecar.Get(ctx, ns, name, container)
		if err != nil {
			consecutiveErr++
			if consecutiveErr >= 2 {
				return last, err
			}
			time.Sleep(backoff)
			continue
		}
		last = st
		consecutiveErr = 0
		if st.State == "terminated" {
			if st.ExitCode != 0 {
				return st, fmt.Errorf("init container %s exited %d: %s", container, st.ExitCode, st.Error)
			}
			return st, nil
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	return last, fmt.Errorf("init container %s did not complete", container)
}
