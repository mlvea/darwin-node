package engine

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/mlvea/darwin-node/pkg/event"

	corev1 "k8s.io/api/core/v1"
)

// noteInterruptedPods records pod UIDs left on disk by the previous
// process. Recover then deletes those directories. Virtual-kubelet creates
// the same pods again; the UID is how this process knows not to boot a
// Never pod from a clean image.
func (e *Engine) noteInterruptedPods() {
	if e == nil || e.cfg.CacheDir == "" {
		return
	}
	uids := interruptedPodUIDs(e.cfg.CacheDir)
	if len(uids) == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.interrupted == nil {
		e.interrupted = map[string]struct{}{}
	}
	for _, uid := range uids {
		e.interrupted[uid] = struct{}{}
	}
}

func interruptedPodUIDs(cacheDir string) []string {
	podsDir := filepath.Join(cacheDir, "pods")
	fi, err := os.Lstat(podsDir)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return nil
	}
	ents, err := os.ReadDir(podsDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, ent := range ents {
		if !ent.IsDir() {
			continue
		}
		name := ent.Name()
		if name == "" || name == "." || name == ".." || strings.HasPrefix(name, "darwin-warm-") {
			continue
		}
		out = append(out, name)
	}
	return out
}

// settleInterruptedLocked records a pod that must not boot, or credits a
// restart for one that should. Caller holds e.mu. settled means the pod is
// now in e.pods and Create must not acquire a slot.
func (e *Engine) settleInterruptedLocked(pod *corev1.Pod) (settled bool, warnReason, warnMsg string) {
	if pod == nil {
		return false, "", ""
	}
	uid := string(pod.UID)
	_, fromDisk := e.interrupted[uid]
	if !fromDisk && !podPreviouslyStarted(pod) {
		return false, "", ""
	}
	delete(e.interrupted, uid)
	if podTerminal(pod) || !restartsAfterNodeDeath(pod.Spec.RestartPolicy) {
		reason, msg := e.insertSettledLocked(pod)
		if reason == event.ReasonNodeRestarted {
			return true, reason, msg
		}
		return true, "", ""
	}
	if e.restartCredit == nil {
		e.restartCredit = map[string]int32{}
	}
	e.restartCredit[uid] = priorRestartCount(pod) + 1
	return false, "", ""
}

func (e *Engine) takeRestartCreditLocked(uid string) int32 {
	if e.restartCredit == nil {
		return 0
	}
	n := e.restartCredit[uid]
	delete(e.restartCredit, uid)
	return n
}

func (e *Engine) insertSettledLocked(pod *corev1.Pod) (reason, message string) {
	cp := pod.DeepCopy()
	phase := cp.Status.Phase
	reason = cp.Status.Reason
	message = cp.Status.Message
	var exitCode *int32
	if phase != corev1.PodSucceeded && phase != corev1.PodFailed {
		phase = corev1.PodFailed
		reason = event.ReasonNodeRestarted
		message = "node restarted while the pod was running; the workload was not started again"
	} else if phase == corev1.PodFailed {
		if code, termReason, termMsg := terminatedExit(cp); code != nil {
			exitCode = code
			if termReason != "" {
				reason = termReason
			}
			if message == "" {
				message = termMsg
			}
		}
	}
	if reason == "" {
		if phase == corev1.PodSucceeded {
			reason = "Completed"
		} else {
			reason = "Failed"
		}
	}
	rec := &podRecord{
		pod:          cp,
		phase:        phase,
		reason:       reason,
		message:      message,
		restartCount: priorRestartCount(cp),
		initialized:  true,
	}
	if phase == corev1.PodFailed {
		rec.guestExit = exitCode
	}
	if cp.Status.StartTime != nil {
		st := *cp.Status.StartTime
		rec.startedAt = &st
	}
	e.pods[Key(cp.Namespace, cp.Name)] = rec
	return reason, message
}

func podTerminal(pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodSucceeded, corev1.PodFailed:
		return true
	default:
		return false
	}
}

func restartsAfterNodeDeath(p corev1.RestartPolicy) bool {
	return p != corev1.RestartPolicyNever
}

func podPreviouslyStarted(pod *corev1.Pod) bool {
	switch pod.Status.Phase {
	case corev1.PodRunning, corev1.PodSucceeded, corev1.PodFailed:
		return true
	}
	return containerWasStarted(pod.Status.ContainerStatuses) || containerWasStarted(pod.Status.InitContainerStatuses)
}

func containerWasStarted(ss []corev1.ContainerStatus) bool {
	for _, cs := range ss {
		if cs.RestartCount > 0 || cs.State.Running != nil || cs.State.Terminated != nil {
			return true
		}
		if cs.Started != nil && *cs.Started {
			return true
		}
	}
	return false
}

func priorRestartCount(pod *corev1.Pod) int32 {
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return 0
	}
	name := pod.Spec.Containers[0].Name
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == name {
			return cs.RestartCount
		}
	}
	return 0
}

func terminatedExit(pod *corev1.Pod) (code *int32, reason, message string) {
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return nil, "", ""
	}
	name := pod.Spec.Containers[0].Name
	for _, cs := range pod.Status.ContainerStatuses {
		term := cs.State.Terminated
		if cs.Name != name || term == nil {
			continue
		}
		c := term.ExitCode
		return &c, term.Reason, term.Message
	}
	return nil, "", ""
}
