package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/hostport"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

func k8sProbe(p *corev1.Probe) (guest.ProbeReq, bool) {
	if p == nil {
		return guest.ProbeReq{}, false
	}
	timeout := time.Duration(p.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = time.Second
	}
	h := p.ProbeHandler
	switch {
	case h.Exec != nil:
		return guest.ProbeReq{Type: guest.ProbeExec, Argv: h.Exec.Command, Timeout: timeout}, true
	case h.HTTPGet != nil:
		host := h.HTTPGet.Host
		port := h.HTTPGet.Port.IntValue()
		path := h.HTTPGet.Path
		if path == "" {
			path = "/"
		}
		scheme := string(h.HTTPGet.Scheme)
		if scheme == "" {
			scheme = "http"
		}
		if host == "" {
			host = "127.0.0.1"
		}
		url := fmt.Sprintf("%s://%s:%d%s", scheme, host, port, path)
		hdr := map[string]string{}
		for _, hh := range h.HTTPGet.HTTPHeaders {
			hdr[hh.Name] = hh.Value
		}
		return guest.ProbeReq{Type: guest.ProbeHTTP, URL: url, Host: host, Port: port, Headers: hdr, Timeout: timeout}, true
	case h.TCPSocket != nil:
		host := h.TCPSocket.Host
		if host == "" {
			host = "127.0.0.1"
		}
		return guest.ProbeReq{Type: guest.ProbeTCP, Host: host, Port: h.TCPSocket.Port.IntValue(), Timeout: timeout}, true
	default:
		return guest.ProbeReq{}, false
	}
}

func (e *Engine) runHook(ctx context.Context, rec *podRecord, cmd []string, what string) error {
	rec.mu.Lock()
	cli := rec.agent
	rec.mu.Unlock()
	if cli == nil {
		return fmt.Errorf("%s: guest agent not connected", what)
	}
	_, stderr, code, err := cli.Exec(ctx, guest.ExecReq{Argv: cmd})
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if code != 0 {
		return fmt.Errorf("%s: exit %d: %s", what, code, stderr)
	}
	return nil
}

func probeTiming(p *corev1.Probe) (initial, period time.Duration, failTh int32) {
	period = 10 * time.Second
	failTh = 3
	if p == nil {
		return 0, period, failTh
	}
	if p.PeriodSeconds > 0 {
		period = time.Duration(p.PeriodSeconds) * time.Second
	}
	if p.InitialDelaySeconds > 0 {
		initial = time.Duration(p.InitialDelaySeconds) * time.Second
	}
	if p.FailureThreshold > 0 {
		failTh = p.FailureThreshold
	}
	return initial, period, failTh
}

func probeSuccessThreshold(p *corev1.Probe) int32 {
	if p != nil && p.SuccessThreshold > 0 {
		return p.SuccessThreshold
	}
	return 1
}

// vmProbeSched is when each VM probe last started. A zero time means the
// next sample is due once the initial delay has elapsed. A restart clears it
// so the new container is probed immediately.
type vmProbeSched struct {
	startup time.Time
	live    time.Time
	ready   time.Time
}

// probeDue reports whether a probe should run at now, and the next time it
// should be considered. The first sample after anchor+initial is immediate.
// Later samples wait period.
func probeDue(now, anchor, last time.Time, initial, period time.Duration) (bool, time.Time) {
	if period <= 0 {
		period = 10 * time.Second
	}
	if initial > 0 && !anchor.IsZero() {
		start := anchor.Add(initial)
		if now.Before(start) {
			return false, start
		}
	}
	if last.IsZero() {
		return true, now
	}
	next := last.Add(period)
	if now.Before(next) {
		return false, next
	}
	return true, now
}

func clampProbeWait(d time.Duration) time.Duration {
	if d < time.Millisecond {
		return time.Millisecond
	}
	return d
}

// nextProbeDelay is how long the watch sleeps. It wakes for the soonest probe
// deadline and at least every ProbeInterval, so exits and backoff are not
// stuck behind a long period.
func (e *Engine) nextProbeDelay(rec *podRecord, sched *vmProbeSched, interval time.Duration) time.Duration {
	if interval <= 0 {
		interval = 10 * time.Second
	}
	now := e.now()
	wait := interval
	tighten := func(next time.Time) {
		if next.IsZero() {
			return
		}
		d := next.Sub(now)
		if d < 0 {
			d = 0
		}
		if d < wait {
			wait = d
		}
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.pod == nil {
		return clampProbeWait(wait)
	}
	running := rec.phase == corev1.PodRunning
	if running && rec.agent != nil && len(rec.pod.Spec.Containers) > 0 && sched != nil {
		c0 := rec.pod.Spec.Containers[0]
		var anchor time.Time
		if !rec.startupSince.IsZero() {
			anchor = rec.startupSince
		} else if rec.startedAt != nil {
			anchor = rec.startedAt.Time
		}
		if !rec.startupDone && hasStartupProbe(c0) {
			initial, period, _ := probeTiming(c0.StartupProbe)
			_, next := probeDue(now, anchor, sched.startup, initial, period)
			tighten(next)
		} else {
			if hasLivenessProbe(c0) {
				initial, period, _ := probeTiming(c0.LivenessProbe)
				_, next := probeDue(now, anchor, sched.live, initial, period)
				tighten(next)
			}
			if hasReadinessProbe(c0) {
				initial, period, _ := probeTiming(c0.ReadinessProbe)
				_, next := probeDue(now, anchor, sched.ready, initial, period)
				tighten(next)
			}
		}
	}
	if len(rec.pod.Spec.Containers) > 1 && (rec.phase == corev1.PodPending || running) {
		byName := make(map[string]sidecar.Status, len(rec.sidecarStatuses))
		for _, st := range rec.sidecarStatuses {
			byName[st.Name] = st
		}
		for _, c := range rec.pod.Spec.Containers[1:] {
			s, ok := byName[c.Name]
			if !ok || s.State != "running" {
				continue
			}
			var lastStart, lastLive, lastReady time.Time
			startDone := !hasStartupProbe(c)
			if ps := rec.sidecarStart[c.Name]; ps != nil && (s.StartedAt.IsZero() || ps.at.Equal(s.StartedAt)) {
				lastStart = ps.last
				lastLive = ps.liveLast
				lastReady = ps.readyLast
				startDone = ps.done || startDone
			}
			if hasStartupProbe(c) && !startDone {
				initial, period, _ := probeTiming(c.StartupProbe)
				_, next := probeDue(now, s.StartedAt, lastStart, initial, period)
				tighten(next)
				continue
			}
			if hasLivenessProbe(c) {
				initial, period, _ := probeTiming(c.LivenessProbe)
				_, next := probeDue(now, s.StartedAt, lastLive, initial, period)
				tighten(next)
			}
			if hasReadinessProbe(c) {
				initial, period, _ := probeTiming(c.ReadinessProbe)
				_, next := probeDue(now, s.StartedAt, lastReady, initial, period)
				tighten(next)
			}
		}
	}
	return clampProbeWait(wait)
}

func (e *Engine) watch(ctx context.Context, rec *podRecord) {
	interval := e.cfg.ProbeInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	var liveFails int32
	var sched vmProbeSched
	// The first pass runs immediately and checks the pod context before
	// Create or Start. A call that has already entered Create or Start
	// does not observe cancellation. Delete waits for this goroutine, then
	// removes whatever that call started. Later passes wait on the probe
	// clock, not on a single ticker, so each probe keeps its own period.
	step := func() bool {
		if ctx.Err() != nil {
			return false
		}
		if !e.noteVMDeath(rec) || !e.enforceVolumeContract(ctx, rec) {
			return false
		}
		e.refreshSidecarStatus(ctx, rec)
		return e.noteSidecarExit(rec) && e.restartExitedSidecars(ctx, rec) && e.tickSidecarStartup(ctx, rec) && e.tickSidecarLiveness(ctx, rec) && e.tickSidecarReadiness(ctx, rec) && e.tickProbes(ctx, rec, &sched, &liveFails)
	}
	if !step() {
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		timer := e.newProbeTimer(e.nextProbeDelay(rec, &sched, interval))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C():
			timer.Stop()
			if !step() {
				return
			}
		}
	}
}

// noteVMDeath fails a Running pod whose machine has stopped or entered the
// runtime's error state. The watch goroutine is the only caller, including
// around restartVM, so a liveness restart's brief Stopped state is not
// observed. Delete sets Succeeded before it stops the machine; failWith
// leaves that phase alone.
func (e *Engine) noteVMDeath(rec *podRecord) bool {
	rec.mu.Lock()
	phase := rec.phase
	var st types.VMState
	if rec.machine != nil {
		st = rec.machine.Status().State
	}
	rec.mu.Unlock()
	if phase != corev1.PodRunning {
		return true
	}
	switch st {
	case types.VMStopped, types.VMFailed:
		e.failWith(rec, "VM"+string(st), fmt.Errorf("macOS VM %s unexpectedly", st))
		return false
	default:
		return true
	}
}

func (e *Engine) tickProbes(ctx context.Context, rec *podRecord, sched *vmProbeSched, liveFails *int32) bool {
	if !e.noteVMDeath(rec) {
		return false
	}
	rec.mu.Lock()
	pod := rec.pod.DeepCopy()
	cli := rec.agent
	phase := rec.phase
	var started time.Time
	if !rec.startupSince.IsZero() {
		started = rec.startupSince
	} else if rec.startedAt != nil {
		started = rec.startedAt.Time
	}
	startupDone := rec.startupDone
	rec.mu.Unlock()
	if cli == nil || phase != corev1.PodRunning {
		return true
	}
	c0 := pod.Spec.Containers[0]
	now := e.now()

	// startupDone lives on the record so a liveness restart can clear it.
	// A local flag would stay true and leave Started set across that restart.
	if !startupDone {
		if req, ok := k8sProbe(c0.StartupProbe); ok {
			initial, period, failTh := probeTiming(c0.StartupProbe)
			due, _ := probeDue(now, started, sched.startup, initial, period)
			if !due {
				if initial > 0 && !started.IsZero() && now.Before(started.Add(initial)) {
					rec.mu.Lock()
					rec.ready = false
					rec.reason = "StartupProbe"
					rec.message = "initial delay"
					rec.mu.Unlock()
				}
				return true
			}
			sched.startup = now
			res, err := cli.Probe(ctx, req)
			if err != nil || !res.OK {
				rec.mu.Lock()
				rec.startupFails++
				fails := rec.startupFails
				rec.ready = false
				rec.reason = "StartupProbe"
				rec.message = probeMsg(res, err)
				rec.mu.Unlock()
				if fails >= failTh {
					e.fail(rec, fmt.Errorf("startup probe failed: %s", probeMsg(res, err)))
					return false
				}
				return true
			}
		}
		rec.mu.Lock()
		rec.startupDone = true
		rec.startupFails = 0
		if _, hasReady := k8sProbe(c0.ReadinessProbe); !hasReady {
			rec.ready = true
			if rec.reason == "StartupProbe" {
				rec.reason = ""
				rec.message = ""
			}
		}
		rec.mu.Unlock()
	}

	var wg sync.WaitGroup

	if req, ok := k8sProbe(c0.ReadinessProbe); ok {
		initial, period, _ := probeTiming(c0.ReadinessProbe)
		if due, _ := probeDue(now, started, sched.ready, initial, period); due {
			sched.ready = now
			req := req
			wg.Add(1)
			go func() {
				defer wg.Done()
				rctx, cancel := probeContext(ctx, req.Timeout)
				defer cancel()
				res, err := cli.Probe(rctx, req)
				ok := err == nil && res.OK
				rec.mu.Lock()
				rec.ready = ok
				if !ok {
					rec.reason = "ReadinessProbe"
					rec.message = probeMsg(res, err)
				} else {
					rec.reason = ""
					rec.message = ""
				}
				rec.mu.Unlock()
			}()
		}
	}

	var liveRes guest.ProbeRes
	var liveErr error
	var liveRan bool
	var liveFailTh int32
	if req, ok := k8sProbe(c0.LivenessProbe); ok {
		initial, period, failTh := probeTiming(c0.LivenessProbe)
		liveFailTh = failTh
		if due, _ := probeDue(now, started, sched.live, initial, period); due {
			sched.live = now
			req := req
			liveRan = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				lctx, cancel := probeContext(ctx, req.Timeout)
				defer cancel()
				liveRes, liveErr = cli.Probe(lctx, req)
			}()
		}
	}
	wg.Wait()
	if !liveRan {
		return true
	}
	res, err := liveRes, liveErr
	if err != nil || !res.OK {
		// Cancellation is Delete or Close, not a failed probe. Starting the
		// VM here races the slot release.
		if ctx.Err() != nil || !podActive(rec) {
			return false
		}
		e.events.Warn(ctx, event.ReasonProbeFailed, probeMsg(res, err))
		*liveFails++
		if *liveFails < liveFailTh {
			return true
		}
		*liveFails = 0
		policy := pod.Spec.RestartPolicy
		if policy == "" {
			policy = corev1.RestartPolicyAlways
		}
		if policy == corev1.RestartPolicyAlways || policy == corev1.RestartPolicyOnFailure {
			rec.mu.Lock()
			rec.restartCount++
			n := rec.restartCount
			rec.mu.Unlock()
			if n > 5 {
				e.fail(rec, fmt.Errorf("liveness probe failed (restarts=%d): %s", n, probeMsg(res, err)))
				return false
			}
			if rerr := e.restartVM(ctx, rec); rerr != nil {
				if ctx.Err() != nil || !podActive(rec) {
					return false
				}
				e.fail(rec, fmt.Errorf("liveness restart: %w", rerr))
				return false
			}
			*sched = vmProbeSched{}
			return true
		}
		e.fail(rec, fmt.Errorf("liveness probe failed: %s", probeMsg(res, err)))
		return false
	}
	*liveFails = 0
	return true
}

func hasStartupProbe(c corev1.Container) bool {
	_, ok := k8sProbe(c.StartupProbe)
	return ok
}

func hasLivenessProbe(c corev1.Container) bool {
	_, ok := k8sProbe(c.LivenessProbe)
	return ok
}

func hasReadinessProbe(c corev1.Container) bool {
	_, ok := k8sProbe(c.ReadinessProbe)
	return ok
}

func probeContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = time.Second
	}
	return context.WithTimeout(parent, timeout)
}

func probeMsg(res guest.ProbeRes, err error) string {
	if err != nil {
		return err.Error()
	}
	if res.Message != "" {
		return res.Message
	}
	if !res.OK {
		return "probe failed"
	}
	return ""
}

func hostPortReservations(pod *corev1.Pod) []hostport.Mapping {
	var out []hostport.Mapping
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.HostPort == 0 {
				continue
			}
			out = append(out, hostport.Mapping{
				HostIP:        p.HostIP,
				HostPort:      int(p.HostPort),
				ContainerPort: int(p.ContainerPort),
				Protocol:      string(p.Protocol),
			})
		}
	}
	return out
}

func hostPortMaps(pod *corev1.Pod, ip string) []hostport.Mapping {
	var out []hostport.Mapping
	if ip == "" {
		return out
	}
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.HostPort == 0 {
				continue
			}
			out = append(out, hostport.Mapping{
				HostIP:        p.HostIP,
				HostPort:      int(p.HostPort),
				PodIP:         ip,
				ContainerPort: int(p.ContainerPort),
				Protocol:      string(p.Protocol),
			})
		}
	}
	return out
}
