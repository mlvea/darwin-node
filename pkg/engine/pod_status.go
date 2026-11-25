package engine

import (
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/types"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// failedExitLocked is the terminated code and reason for a Failed pod.
// A delete that stopped the workload before it exited publishes that kill
// code (137 or 143). Otherwise a guest Exit notify wins, including 0. The
// runtime's code is used when the guest sent none. Otherwise the code is 1
// and the reason is the host's (VMStopped, VMFailed, Failed).
func failedExitLocked(rec *podRecord, vmStatus types.VMStatus) (int32, string) {
	if rec.killedCode != nil {
		reason := rec.reason
		if reason == "" {
			reason = "Error"
		}
		return *rec.killedCode, reason
	}
	reason := rec.reason
	if rec.guestExit != nil {
		code := guest.ClampExitCode(*rec.guestExit)
		if rec.guestExitReason != "" {
			reason = rec.guestExitReason
		}
		return code, reason
	}
	if vmStatus.ExitCode != nil {
		return *vmStatus.ExitCode, reason
	}
	return 1, reason
}

// Get returns a pod with status filled.
func (e *Engine) Get(namespace, name string) (*corev1.Pod, error) {
	e.mu.RLock()
	rec, ok := e.pods[Key(namespace, name)]
	e.mu.RUnlock()
	if !ok {
		return nil, errdefs.NotFound("pod not found")
	}
	return e.podFrom(rec), nil
}

// WorkloadExitCode is the last guest Exit code recorded for this pod.
// ok is false until that frame arrives. A VM restart clears it.
func (e *Engine) WorkloadExitCode(namespace, name string) (int32, bool) {
	if e == nil {
		return 0, false
	}
	e.mu.RLock()
	rec := e.pods[Key(namespace, name)]
	e.mu.RUnlock()
	if rec == nil {
		return 0, false
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.guestExit == nil {
		return 0, false
	}
	return *rec.guestExit, true
}

// List returns all pods.
func (e *Engine) List() []*corev1.Pod {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*corev1.Pod, 0, len(e.pods))
	for _, rec := range e.pods {
		out = append(out, e.podFrom(rec))
	}
	return out
}

func (e *Engine) podFrom(rec *podRecord) *corev1.Pod {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	p := rec.pod.DeepCopy()
	p.Status = e.buildStatusLocked(rec)
	return p
}

func (e *Engine) buildStatusLocked(rec *podRecord) corev1.PodStatus {
	st := rec.machine
	ip := rec.pod.Status.PodIP
	var vmState types.VMState
	var vmStatus types.VMStatus
	if st != nil {
		vmStatus = st.Status()
		vmState = vmStatus.State
		// Status.IP is the address the runtime recorded (DHCP, ARP, or the
		// fake runtime). A loopback or link-local value is not published.
		if canonical, ferr := forwardableIP(vmStatus.IP); ferr == nil {
			ip = canonical
		}
	}
	if canonical, ferr := forwardableIP(ip); ferr == nil {
		ip = canonical
	} else {
		ip = ""
	}
	started := rec.startupDone && rec.agentOK && rec.phase == corev1.PodRunning
	vmReady := rec.phase == corev1.PodRunning && rec.ready && rec.agentOK
	ready := vmReady && sidecarsRunningLocked(rec)

	cs := corev1.ContainerStatus{
		Name:         rec.pod.Spec.Containers[0].Name,
		Image:        rec.pod.Spec.Containers[0].Image,
		Ready:        vmReady,
		Started:      &started,
		ContainerID:  "vz://" + rec.pod.Spec.Containers[0].Name,
		RestartCount: rec.restartCount,
	}
	switch rec.phase {
	case corev1.PodPending:
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: guest.SanitizeGuestText(rec.reason), Message: guest.SanitizeGuestText(rec.message)}
	case corev1.PodFailed:
		code, termReason := failedExitLocked(rec, vmStatus)
		cs.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: code, Reason: guest.SanitizeGuestText(termReason), Message: guest.SanitizeGuestText(rec.message)}
	case corev1.PodSucceeded:
		cs.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 0, Reason: guest.SanitizeGuestText(rec.reason), Message: guest.SanitizeGuestText(rec.message)}
	default:
		start := metav1.Now()
		if rec.startedAt != nil {
			start = *rec.startedAt
		}
		cs.State.Running = &corev1.ContainerStateRunning{StartedAt: start}
	}
	_ = vmState

	now := metav1.NewTime(e.now())
	startTime := rec.startedAt
	statuses := []corev1.ContainerStatus{cs}
	if rec.pod != nil && len(rec.pod.Spec.Containers) > 1 {
		statuses = append(statuses, sidecarContainerStatuses(rec.pod.Spec.Containers[1:], rec.sidecarStatuses, rec.sidecarRestarts, rec.sidecarLastExit, rec.sidecarStart, rec.sidecarWait)...)
	}
	startupBlock := ""
	if ready {
		if msg, blocked := sidecarStartupBlockLocked(rec); blocked {
			ready = false
			startupBlock = msg
		}
	}
	readinessBlock := ""
	if ready {
		if msg, blocked := sidecarReadinessBlockLocked(rec); blocked {
			ready = false
			readinessBlock = msg
		}
	}
	containersReady := boolCond(corev1.ContainersReady, ready, now, rec)
	podReady := boolCond(corev1.PodReady, ready, now, rec)
	if vmReady && !ready {
		msg := sidecarDownMessageLocked(rec)
		reason := "SidecarNotRunning"
		if startupBlock != "" {
			reason = "StartupProbe"
			msg = startupBlock
		} else if readinessBlock != "" {
			reason = "ReadinessProbe"
			msg = readinessBlock
		}
		containersReady.Reason = reason
		containersReady.Message = guest.SanitizeGuestText(msg)
		podReady.Reason = reason
		podReady.Message = guest.SanitizeGuestText(msg)
	}
	return corev1.PodStatus{
		Phase:                 rec.phase,
		Reason:                guest.SanitizeGuestText(rec.reason),
		Message:               guest.SanitizeGuestText(rec.message),
		HostIP:                e.hostIP,
		PodIP:                 ip,
		StartTime:             startTime,
		ContainerStatuses:     statuses,
		InitContainerStatuses: copyContainerStatuses(rec.initStatuses),
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: stampTransition(rec, corev1.PodScheduled, corev1.ConditionTrue, now)},
			boolCond(corev1.PodInitialized, rec.initialized || len(rec.pod.Spec.InitContainers) == 0, now, rec),
			containersReady,
			podReady,
		},
	}
}

func boolCond(t corev1.PodConditionType, ok bool, now metav1.Time, rec *podRecord) corev1.PodCondition {
	st := corev1.ConditionFalse
	reason := guest.SanitizeGuestText(rec.reason)
	msg := guest.SanitizeGuestText(rec.message)
	if ok {
		st = corev1.ConditionTrue
		reason = "Ready"
		msg = "guest agent ready"
	} else if reason == "" {
		reason = "NotReady"
	}
	return corev1.PodCondition{Type: t, Status: st, LastTransitionTime: stampTransition(rec, t, st, now), Reason: reason, Message: msg}
}

// stampTransition keeps LastTransitionTime until the condition status
// changes. A Deployment reads PodReady's transition time for
// minReadySeconds; rewriting it on every status read stalls the rollout.
func stampTransition(rec *podRecord, typ corev1.PodConditionType, st corev1.ConditionStatus, now metav1.Time) metav1.Time {
	if rec == nil {
		return now
	}
	if rec.transitions == nil {
		rec.transitions = map[corev1.PodConditionType]condTransition{}
	}
	if prev, ok := rec.transitions[typ]; ok && prev.status == st && !prev.at.IsZero() {
		return prev.at
	}
	if now.IsZero() {
		now = metav1.Now()
	}
	rec.transitions[typ] = condTransition{status: st, at: now}
	return now
}

func copyContainerStatuses(in []corev1.ContainerStatus) []corev1.ContainerStatus {
	if len(in) == 0 {
		return nil
	}
	out := make([]corev1.ContainerStatus, len(in))
	for i := range in {
		out[i] = in[i]
		if in[i].Started != nil {
			v := *in[i].Started
			out[i].Started = &v
		}
		if in[i].State.Terminated != nil {
			cp := *in[i].State.Terminated
			out[i].State.Terminated = &cp
		}
		if in[i].State.Running != nil {
			cp := *in[i].State.Running
			out[i].State.Running = &cp
		}
		if in[i].State.Waiting != nil {
			cp := *in[i].State.Waiting
			out[i].State.Waiting = &cp
		}
	}
	return out
}
