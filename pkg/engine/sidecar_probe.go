package engine

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

// sidecarStartState is the probe result for one sidecar generation.
// at is the container start time the result belongs to. A new start time
// means the container was restarted and every probe starts again.
// last, liveLast, and readyLast are when that probe last started. Zero
// means the next sample is due once the initial delay has elapsed.
type sidecarStartState struct {
	at    time.Time
	done  bool
	fails int32
	oks   int32
	last  time.Time
	msg   string

	liveFails int32
	liveLast  time.Time

	ready      bool
	readyOKs   int32
	readyFails int32
	readyMsg   string
	readyLast  time.Time
}

func sidecarStartupPassed(st *sidecarStartState, s sidecar.Status) bool {
	if st == nil || !st.done {
		return false
	}
	if !s.StartedAt.IsZero() && !st.at.Equal(s.StartedAt) {
		return false
	}
	return true
}

func sidecarReadyPassed(st *sidecarStartState, s sidecar.Status) bool {
	if st == nil || !st.ready {
		return false
	}
	if !s.StartedAt.IsZero() && !st.at.Equal(s.StartedAt) {
		return false
	}
	return true
}

// sidecarProbeStateLocked returns the probe state for this container start.
// The caller holds rec.mu. A new start time drops the previous generation.
func sidecarProbeStateLocked(rec *podRecord, name string, started time.Time) *sidecarStartState {
	if rec.sidecarStart == nil {
		rec.sidecarStart = map[string]*sidecarStartState{}
	}
	cur := rec.sidecarStart[name]
	if cur == nil || !cur.at.Equal(started) {
		cur = &sidecarStartState{at: started}
		rec.sidecarStart[name] = cur
	}
	return cur
}

// sidecarStartupBlockLocked reports a running sidecar whose startup probe
// has not passed. The caller holds rec.mu. A sidecar that is not running is
// left to the SidecarNotRunning condition.
func sidecarStartupBlockLocked(rec *podRecord) (string, bool) {
	if rec.pod == nil || len(rec.pod.Spec.Containers) < 2 {
		return "", false
	}
	byName := make(map[string]sidecar.Status, len(rec.sidecarStatuses))
	for _, st := range rec.sidecarStatuses {
		byName[st.Name] = st
	}
	for _, c := range rec.pod.Spec.Containers[1:] {
		if !hasStartupProbe(c) {
			continue
		}
		s, ok := byName[c.Name]
		if !ok || s.State != "running" {
			continue
		}
		if sidecarStartupPassed(rec.sidecarStart[c.Name], s) {
			continue
		}
		msg := "startup probe"
		if cur := rec.sidecarStart[c.Name]; cur != nil && cur.msg != "" {
			msg = cur.msg
		}
		return fmt.Sprintf("sidecar %s %s", c.Name, msg), true
	}
	return "", false
}

// sidecarReadinessBlockLocked reports a running sidecar whose readiness
// probe has not passed. The caller holds rec.mu. A sidecar that is not
// running is left to the SidecarNotRunning condition.
func sidecarReadinessBlockLocked(rec *podRecord) (string, bool) {
	if rec.pod == nil || len(rec.pod.Spec.Containers) < 2 {
		return "", false
	}
	byName := make(map[string]sidecar.Status, len(rec.sidecarStatuses))
	for _, st := range rec.sidecarStatuses {
		byName[st.Name] = st
	}
	for _, c := range rec.pod.Spec.Containers[1:] {
		if !hasReadinessProbe(c) {
			continue
		}
		s, ok := byName[c.Name]
		if !ok || s.State != "running" {
			continue
		}
		if sidecarReadyPassed(rec.sidecarStart[c.Name], s) {
			continue
		}
		msg := "readiness probe"
		if cur := rec.sidecarStart[c.Name]; cur != nil && cur.readyMsg != "" {
			msg = cur.readyMsg
		}
		return fmt.Sprintf("sidecar %s %s", c.Name, msg), true
	}
	return "", false
}

// tickSidecarStartup runs startup probes for hybrid sidecars. The VM's
// probes stay on the guest agent; these run against the host container.
// Past the failure threshold the container is stopped. The pod restart
// policy then applies, the same as a container exit: Always and OnFailure
// start it again and the pod stays up, Never does not and a non-zero kill
// fails the pod. Kubelet kills a container whose startup probe fails and
// restarts it under its restart policy; it does not fail the pod outright.
func (e *Engine) tickSidecarStartup(ctx context.Context, rec *podRecord) bool {
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
	statuses := append([]sidecar.Status(nil), rec.sidecarStatuses...)
	rec.mu.Unlock()

	byName := make(map[string]sidecar.Status, len(statuses))
	for _, st := range statuses {
		byName[st.Name] = st
	}
	for _, c := range pod.Spec.Containers[1:] {
		if !hasStartupProbe(c) {
			continue
		}
		s, ok := byName[c.Name]
		if !ok || s.State != "running" {
			continue
		}
		now := e.now()
		rec.mu.Lock()
		cur := sidecarProbeStateLocked(rec, c.Name, s.StartedAt)
		if cur.done {
			rec.mu.Unlock()
			continue
		}
		initial, period, failTh := probeTiming(c.StartupProbe)
		due, _ := probeDue(now, s.StartedAt, cur.last, initial, period)
		if !due {
			if initial > 0 && !s.StartedAt.IsZero() && now.Before(s.StartedAt.Add(initial)) {
				cur.msg = "initial delay"
			}
			rec.mu.Unlock()
			continue
		}
		cur.last = now
		rec.mu.Unlock()

		if ctx.Err() != nil {
			return false
		}
		okProbe, msg := runSidecarProbe(ctx, e.sidecar, pod.Namespace, pod.Name, c.Name, c.StartupProbe)
		if ctx.Err() != nil {
			return false
		}
		rec.mu.Lock()
		if okProbe {
			cur.oks++
			cur.fails = 0
			if cur.oks >= probeSuccessThreshold(c.StartupProbe) {
				cur.done = true
				cur.msg = ""
			}
			rec.mu.Unlock()
			continue
		}
		cur.oks = 0
		cur.fails++
		fails := cur.fails
		cur.msg = msg
		rec.mu.Unlock()
		if fails >= failTh {
			if ctx.Err() != nil || !podActive(rec) {
				return false
			}
			// Stop, then let the exit path apply the restart policy. The
			// container's own exit code is whatever the runtime records.
			err := e.stopSidecarContainer(ctx, pod.Namespace, pod.Name, c.Name)
			if ctx.Err() != nil || !podActive(rec) {
				return false
			}
			if err != nil {
				e.fail(rec, fmt.Errorf("sidecar %s startup probe failed: %s", c.Name, msg))
				return false
			}
			e.events.Warn(ctx, event.ReasonProbeFailed, fmt.Sprintf("sidecar %s startup probe failed: %s", c.Name, msg))
			e.refreshSidecarStatus(ctx, rec)
			if !e.noteSidecarExit(rec) || !e.restartExitedSidecars(ctx, rec) {
				return false
			}
			return true
		}
	}
	return true
}

func (e *Engine) sidecarProbePod(rec *podRecord) (*corev1.Pod, map[string]sidecar.Status, bool) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.pod == nil || len(rec.pod.Spec.Containers) < 2 {
		return nil, nil, false
	}
	if rec.phase != corev1.PodPending && rec.phase != corev1.PodRunning {
		return nil, nil, false
	}
	pod := rec.pod.DeepCopy()
	byName := make(map[string]sidecar.Status, len(rec.sidecarStatuses))
	for _, st := range rec.sidecarStatuses {
		byName[st.Name] = st
	}
	return pod, byName, true
}

func sidecarStartupOpen(rec *podRecord, c corev1.Container, s sidecar.Status) bool {
	if !hasStartupProbe(c) {
		return false
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return !sidecarStartupPassed(rec.sidecarStart[c.Name], s)
}

// tickSidecarLiveness kills a sidecar once its liveness probe has failed
// failureThreshold times in a row. The pod restart policy and the crash
// backoff then apply, the same as an exit. The VM is not restarted.
// Liveness waits until the startup probe has passed. Cancelling the watch
// is not a failure and does not stop the container.
func (e *Engine) tickSidecarLiveness(ctx context.Context, rec *podRecord) bool {
	pod, byName, ok := e.sidecarProbePod(rec)
	if !ok {
		return true
	}
	for _, c := range pod.Spec.Containers[1:] {
		if !hasLivenessProbe(c) {
			continue
		}
		s, found := byName[c.Name]
		if !found || s.State != "running" {
			continue
		}
		if sidecarStartupOpen(rec, c, s) {
			continue
		}
		now := e.now()
		initial, period, failTh := probeTiming(c.LivenessProbe)
		rec.mu.Lock()
		cur := sidecarProbeStateLocked(rec, c.Name, s.StartedAt)
		due, _ := probeDue(now, s.StartedAt, cur.liveLast, initial, period)
		if !due {
			rec.mu.Unlock()
			continue
		}
		cur.liveLast = now
		rec.mu.Unlock()
		if ctx.Err() != nil {
			return false
		}
		okProbe, msg := runSidecarProbe(ctx, e.sidecar, pod.Namespace, pod.Name, c.Name, c.LivenessProbe)
		if ctx.Err() != nil {
			return false
		}
		rec.mu.Lock()
		if okProbe {
			// A success clears the streak. Liveness successThreshold is 1.
			cur.liveFails = 0
			rec.mu.Unlock()
			continue
		}
		cur.liveFails++
		fails := cur.liveFails
		rec.mu.Unlock()
		if fails < failTh {
			continue
		}
		if ctx.Err() != nil || !podActive(rec) {
			return false
		}
		err := e.stopSidecarContainer(ctx, pod.Namespace, pod.Name, c.Name)
		if ctx.Err() != nil || !podActive(rec) {
			return false
		}
		if err != nil {
			e.fail(rec, fmt.Errorf("sidecar %s liveness probe failed: %s", c.Name, msg))
			return false
		}
		e.events.Warn(ctx, event.ReasonProbeFailed, fmt.Sprintf("sidecar %s liveness probe failed: %s", c.Name, msg))
		e.refreshSidecarStatus(ctx, rec)
		if !e.noteSidecarExit(rec) || !e.restartExitedSidecars(ctx, rec) {
			return false
		}
		return true
	}
	return true
}

// tickSidecarReadiness updates the sidecar Ready bit. It does not stop the
// container. The container stays unready until successThreshold successes,
// and a later failureThreshold of failures clears Ready again. Readiness
// waits until the startup probe has passed.
func (e *Engine) tickSidecarReadiness(ctx context.Context, rec *podRecord) bool {
	pod, byName, ok := e.sidecarProbePod(rec)
	if !ok {
		return true
	}
	for _, c := range pod.Spec.Containers[1:] {
		if !hasReadinessProbe(c) {
			continue
		}
		s, found := byName[c.Name]
		if !found || s.State != "running" {
			continue
		}
		if sidecarStartupOpen(rec, c, s) {
			continue
		}
		now := e.now()
		initial, period, failTh := probeTiming(c.ReadinessProbe)
		successTh := probeSuccessThreshold(c.ReadinessProbe)
		rec.mu.Lock()
		cur := sidecarProbeStateLocked(rec, c.Name, s.StartedAt)
		due, _ := probeDue(now, s.StartedAt, cur.readyLast, initial, period)
		if !due {
			if initial > 0 && !s.StartedAt.IsZero() && now.Before(s.StartedAt.Add(initial)) {
				cur.readyMsg = "initial delay"
			}
			rec.mu.Unlock()
			continue
		}
		cur.readyLast = now
		rec.mu.Unlock()
		if ctx.Err() != nil {
			return false
		}
		okProbe, msg := runSidecarProbe(ctx, e.sidecar, pod.Namespace, pod.Name, c.Name, c.ReadinessProbe)
		if ctx.Err() != nil {
			return false
		}
		rec.mu.Lock()
		if okProbe {
			cur.readyOKs++
			cur.readyFails = 0
			if cur.readyOKs >= successTh {
				cur.ready = true
				cur.readyMsg = ""
			}
		} else {
			cur.readyFails++
			cur.readyOKs = 0
			cur.readyMsg = msg
			if cur.readyFails >= failTh {
				cur.ready = false
			}
		}
		rec.mu.Unlock()
	}
	return true
}

func (e *Engine) stopSidecarContainer(ctx context.Context, ns, name, container string) error {
	stopper, ok := e.sidecar.(sidecar.ContainerStopper)
	if !ok {
		return fmt.Errorf("sidecar runtime cannot stop containers")
	}
	return stopper.StopContainer(ctx, ns, name, container)
}

func runSidecarProbe(ctx context.Context, rt sidecar.Runtime, ns, podName, container string, p *corev1.Probe) (bool, string) {
	req, ok := k8sProbe(p)
	if !ok {
		return true, ""
	}
	pctx, cancel := probeContext(ctx, req.Timeout)
	defer cancel()
	switch req.Type {
	case guest.ProbeExec:
		err := rt.Exec(pctx, ns, podName, container, req.Argv, nil)
		if err != nil {
			return false, err.Error()
		}
		return true, ""
	case guest.ProbeHTTP:
		return sidecarHTTPProbe(pctx, req)
	case guest.ProbeTCP:
		return sidecarTCPProbe(pctx, req)
	default:
		return false, "unknown probe type"
	}
}

func sidecarHTTPProbe(ctx context.Context, req guest.ProbeReq) (bool, string) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, req.URL, nil)
	if err != nil {
		return false, err.Error()
	}
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(httpReq)
	if err != nil {
		return false, err.Error()
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return true, ""
}

func sidecarTCPProbe(ctx context.Context, req guest.ProbeReq) (bool, string) {
	host := req.Host
	if host == "" {
		host = "127.0.0.1"
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(req.Port)))
	if err != nil {
		return false, err.Error()
	}
	_ = c.Close()
	return true, ""
}
