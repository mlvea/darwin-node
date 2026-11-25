package engine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/internal/netutil"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/hostport"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func k8sProbe(p *corev1.Probe, ports []corev1.ContainerPort) (guest.ProbeReq, bool) {
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
		port := probePort(h.HTTPGet.Port, ports)
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
		return guest.ProbeReq{Type: guest.ProbeTCP, Host: host, Port: probePort(h.TCPSocket.Port, ports), Timeout: timeout}, true
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

// vmProbeState is the VM container's three probe streaks. It lives on the
// watch, and a liveness restart zeroes it with the schedule.
type vmProbeState struct {
	startup probeStreak
	live    probeStreak
	ready   probeStreak
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
				startDone = ps.startup.passed || startDone
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
	var probes vmProbeState
	var sched vmProbeSched
	// Sidecar probe I/O runs off this goroutine. The watch still applies
	// the results, so a slow sidecar sample cannot hold a VM probe.
	flights := newSideFlights()
	defer flights.stop()
	// The first pass runs immediately and checks the pod context before
	// Create or Start. A call that has already entered Create or Start
	// does not observe cancellation. Delete waits for this goroutine, then
	// removes whatever that call started. Later passes wait on the probe
	// clock, not on a single ticker, so each probe keeps its own period.
	// A finished sidecar probe wakes the loop so its result is applied
	// without waiting out the next period.
	step := func() bool {
		if ctx.Err() != nil {
			return false
		}
		restarted, keep := e.noteVMDeath(ctx, rec)
		if !keep || !e.enforceVolumeContract(ctx, rec) {
			return false
		}
		if restarted {
			probes = vmProbeState{}
			sched = vmProbeSched{}
		}
		e.refreshSidecarStatus(ctx, rec)
		if !e.noteSidecarExit(rec) || !e.restartExitedSidecars(ctx, rec) {
			return false
		}
		if !e.settleSidecarProbes(ctx, rec, flights) {
			return false
		}
		e.launchSidecarProbes(ctx, rec, flights)
		return e.tickProbes(ctx, rec, &sched, &probes)
	}
	if !step() {
		return
	}
	for {
		if ctx.Err() != nil {
			return
		}
		timer := e.newProbeTimer(e.nextProbeDelay(rec, &sched, interval))
		sessionCli := e.agentSession(rec)
		var session <-chan struct{}
		if sessionCli != nil {
			session = sessionCli.Done()
		}
		select {
		case <-ctx.Done():
			stopProbeTimer(timer)
			return
		case <-flights.wake:
			stopProbeTimer(timer)
			if !step() {
				return
			}
		case <-timer.C():
			if !step() {
				return
			}
		case <-session:
			stopProbeTimer(timer)
			if !e.recoverAgent(ctx, rec, sessionCli) {
				return
			}
			// recoverAgent returns without a new client when the VM is
			// already down. Selecting the same closed session again would
			// spin. Note the death here: the next timer is ProbeInterval
			// once the agent is gone, and a stopped VM would sit Running
			// until that interval elapsed.
			rec.mu.Lock()
			cleared := rec.agent == sessionCli
			if cleared {
				rec.agent = nil
			}
			rec.mu.Unlock()
			if !cleared {
				continue
			}
			restarted, keep := e.noteVMDeath(ctx, rec)
			if !keep {
				return
			}
			if restarted {
				probes = vmProbeState{}
				sched = vmProbeSched{}
			}
		}
	}
}

// stopProbeTimer releases a wait. A timer that already fired is drained so
// the value does not sit on the channel.
func stopProbeTimer(timer probeTimer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C():
		default:
		}
	}
}

// noteVMDeath handles a Running pod whose machine has stopped or entered
// the runtime's error state. The watch goroutine is the only caller,
// including around restartVM, so a liveness restart's brief Stopped state
// is not observed. A guest exit code selects the phase: 0 with Never or
// OnFailure is Succeeded, and Always or a non-zero OnFailure starts the VM
// again. A stop with no guest code is still an unexpected death.
// restarted tells the caller to clear its probe streaks. keep is false when
// the watch should stop. Delete sets a terminal phase before it stops the
// machine, and failWith leaves that phase alone.
func (e *Engine) noteVMDeath(ctx context.Context, rec *podRecord) (restarted, keep bool) {
	rec.mu.Lock()
	phase := rec.phase
	var st types.VMState
	if rec.machine != nil {
		st = rec.machine.Status().State
	}
	var code int32
	reported := rec.guestExit != nil
	if reported {
		code = *rec.guestExit
	}
	policy := corev1.RestartPolicyAlways
	if rec.pod != nil && rec.pod.Spec.RestartPolicy != "" {
		policy = rec.pod.Spec.RestartPolicy
	}
	rec.mu.Unlock()
	if phase != corev1.PodRunning {
		return false, true
	}
	switch st {
	case types.VMStopped, types.VMFailed:
	default:
		return false, true
	}
	switch vmExitActionFor(policy, code, reported) {
	case vmExitSucceed:
		e.succeedWorkload(rec)
		return false, false
	case vmExitRestart:
		if err := e.restartVM(ctx, rec); err != nil {
			if ctx.Err() != nil || !podActive(rec) {
				return false, false
			}
			e.fail(rec, fmt.Errorf("workload restart: %w", err))
			return false, false
		}
		rec.mu.Lock()
		rec.restartCount++
		rec.mu.Unlock()
		return true, true
	default:
		e.failWith(rec, "VM"+string(st), fmt.Errorf("macOS VM %s unexpectedly", st))
		return false, false
	}
}

type vmExitKind int

const (
	vmExitFail vmExitKind = iota
	vmExitSucceed
	vmExitRestart
)

// vmExitActionFor is the restart policy for a guest Exit. An empty policy
// is Always, which is the Kubernetes default. A stop that carried no code
// is not a workload result.
func vmExitActionFor(policy corev1.RestartPolicy, code int32, reported bool) vmExitKind {
	if !reported {
		return vmExitFail
	}
	switch policy {
	case "", corev1.RestartPolicyAlways:
		return vmExitRestart
	case corev1.RestartPolicyOnFailure:
		if code == 0 {
			return vmExitSucceed
		}
		return vmExitRestart
	default:
		if code == 0 {
			return vmExitSucceed
		}
		return vmExitFail
	}
}

func (e *Engine) tickProbes(ctx context.Context, rec *podRecord, sched *vmProbeSched, probes *vmProbeState) bool {
	restarted, keep := e.noteVMDeath(ctx, rec)
	if !keep {
		return false
	}
	if restarted {
		*probes = vmProbeState{}
		*sched = vmProbeSched{}
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
	if cli == nil || phase != corev1.PodRunning || probes == nil {
		return true
	}
	c0 := pod.Spec.Containers[0]
	now := e.now()

	// startupDone lives on the record so a liveness restart can clear it.
	// A local flag would stay true and leave Started set across that restart.
	if !startupDone {
		if req, ok := k8sProbe(c0.StartupProbe, c0.Ports); ok {
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
			sctx, cancel := probeContext(ctx, req.Timeout)
			probeStart := time.Now()
			res, err := cli.Probe(sctx, req)
			observeProbe("startup", probeStart, err)
			cancel()
			// Parent cancel is Delete or Close, not a failed probe.
			if ctx.Err() != nil || !podActive(rec) {
				return false
			}
			passed, failed := probes.startup.observe(err == nil && res.OK, probeMsg(res, err), probeSuccessThreshold(c0.StartupProbe), failTh)
			if !passed {
				rec.mu.Lock()
				rec.ready = false
				rec.reason = "StartupProbe"
				rec.message = probes.startup.msg
				rec.mu.Unlock()
				if failed {
					e.fail(rec, fmt.Errorf("startup probe failed: %s", probes.startup.msg))
					return false
				}
				return true
			}
		}
		rec.mu.Lock()
		rec.startupDone = true
		if _, hasReady := k8sProbe(c0.ReadinessProbe, c0.Ports); !hasReady {
			rec.ready = true
			if rec.reason == "StartupProbe" {
				rec.reason = ""
				rec.message = ""
			}
		}
		rec.mu.Unlock()
	}

	var wg sync.WaitGroup

	if req, ok := k8sProbe(c0.ReadinessProbe, c0.Ports); ok {
		initial, period, failTh := probeTiming(c0.ReadinessProbe)
		successTh := probeSuccessThreshold(c0.ReadinessProbe)
		if due, _ := probeDue(now, started, sched.ready, initial, period); due {
			sched.ready = now
			req := req
			wg.Add(1)
			go func() {
				defer wg.Done()
				rctx, cancel := probeContext(ctx, req.Timeout)
				defer cancel()
				probeStart := time.Now()
				res, err := cli.Probe(rctx, req)
				observeProbe("readiness", probeStart, err)
				// Parent cancel is Delete or Close, not a failed probe.
				if ctx.Err() != nil {
					return
				}
				passed, _ := probes.ready.observe(err == nil && res.OK, probeMsg(res, err), successTh, failTh)
				rec.mu.Lock()
				rec.ready = passed
				if passed {
					if rec.reason == "ReadinessProbe" {
						rec.reason = ""
						rec.message = ""
					}
				} else if probes.ready.fails > 0 {
					rec.reason = "ReadinessProbe"
					rec.message = probes.ready.msg
				}
				rec.mu.Unlock()
			}()
		} else if initial > 0 && !started.IsZero() && now.Before(started.Add(initial)) {
			rec.mu.Lock()
			if !rec.ready {
				rec.reason = "ReadinessProbe"
				rec.message = "initial delay"
			}
			rec.mu.Unlock()
		}
	}

	var liveRes guest.ProbeRes
	var liveErr error
	var liveRan bool
	var liveFailTh int32
	if req, ok := k8sProbe(c0.LivenessProbe, c0.Ports); ok {
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
				probeStart := time.Now()
				liveRes, liveErr = cli.Probe(lctx, req)
				observeProbe("liveness", probeStart, liveErr)
			}()
		}
	}
	wg.Wait()
	if !liveRan {
		return true
	}
	// Cancellation is Delete or Close, not a failed probe. Starting the
	// VM here races the slot release.
	if ctx.Err() != nil || !podActive(rec) {
		return false
	}
	res, err := liveRes, liveErr
	okProbe := err == nil && res.OK
	if !okProbe {
		e.events.Warn(ctx, event.ReasonProbeFailed, probeMsg(res, err))
	}
	// Liveness successThreshold is 1. One success clears the streak.
	_, failed := probes.live.observe(okProbe, probeMsg(res, err), 1, liveFailTh)
	if !failed {
		return true
	}
	policy := pod.Spec.RestartPolicy
	if policy == "" {
		policy = corev1.RestartPolicyAlways
	}
	if policy == corev1.RestartPolicyAlways || policy == corev1.RestartPolicyOnFailure {
		// Kubelet CrashLoopBackOff: 10s, doubling, capped at 5m, reset
		// after 10m without a restart. The count is not a lifetime cap.
		delay := e.nextLivenessDelay(rec)
		msg := fmt.Sprintf("back-off %s restarting the failed container", delay)
		rec.mu.Lock()
		if rec.phase == corev1.PodPending || rec.phase == corev1.PodRunning {
			rec.reason = "CrashLoopBackOff"
			rec.message = msg
		}
		rec.mu.Unlock()
		e.events.Warn(ctx, event.ReasonBackOff, msg)
		if werr := e.sleepBackoff(ctx, delay); werr != nil {
			return false
		}
		if ctx.Err() != nil || !podActive(rec) {
			return false
		}
		rec.mu.Lock()
		rec.restartCount++
		rec.mu.Unlock()
		if rerr := e.restartVM(ctx, rec); rerr != nil {
			if ctx.Err() != nil || !podActive(rec) {
				return false
			}
			e.fail(rec, fmt.Errorf("liveness restart: %w", rerr))
			return false
		}
		e.noteLivenessRestarted(rec, delay)
		*probes = vmProbeState{}
		*sched = vmProbeSched{}
		return true
	}
	e.fail(rec, fmt.Errorf("liveness probe failed: %s", probeMsg(res, err)))
	return false
}

const (
	livenessBackoffInitial = 10 * time.Second
	livenessBackoffMax     = 5 * time.Minute
	livenessBackoffReset   = 10 * time.Minute
)

// nextLivenessDelay is the wait before the next liveness restart. The
// first restart waits 10s. Each later restart doubles that, up to 5m.
// Ten minutes since the previous restart resets it to 10s.
func (e *Engine) nextLivenessDelay(rec *podRecord) time.Duration {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.liveBackoff < livenessBackoffInitial {
		return livenessBackoffInitial
	}
	if !rec.liveBackoffAt.IsZero() && e.now().Sub(rec.liveBackoffAt) >= livenessBackoffReset {
		return livenessBackoffInitial
	}
	if rec.liveBackoff > livenessBackoffMax {
		return livenessBackoffMax
	}
	return rec.liveBackoff
}

func (e *Engine) noteLivenessRestarted(rec *podRecord, delay time.Duration) {
	next := delay * 2
	if next > livenessBackoffMax || next <= 0 {
		next = livenessBackoffMax
	}
	rec.mu.Lock()
	rec.liveBackoff = next
	rec.liveBackoffAt = e.now()
	rec.mu.Unlock()
}

func hasStartupProbe(c corev1.Container) bool {
	_, ok := k8sProbe(c.StartupProbe, c.Ports)
	return ok
}

func hasLivenessProbe(c corev1.Container) bool {
	_, ok := k8sProbe(c.LivenessProbe, c.Ports)
	return ok
}

func hasReadinessProbe(c corev1.Container) bool {
	_, ok := k8sProbe(c.ReadinessProbe, c.Ports)
	return ok
}

func probeContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = time.Second
	}
	return context.WithTimeout(parent, timeout)
}

func probePort(port intstr.IntOrString, ports []corev1.ContainerPort) int {
	if port.Type == intstr.String {
		for _, p := range ports {
			if p.Name == port.StrVal && p.ContainerPort > 0 {
				return int(p.ContainerPort)
			}
		}
		return 0
	}
	return port.IntValue()
}

func probeMsg(res guest.ProbeRes, err error) string {
	if err != nil {
		return guest.SanitizeGuestText(err.Error())
	}
	if res.Message != "" {
		return guest.SanitizeGuestText(res.Message)
	}
	if !res.OK {
		return "probe failed"
	}
	return ""
}

func hostPortReservations(pod *corev1.Pod, bindHost string) []hostport.Mapping {
	var out []hostport.Mapping
	if pod == nil {
		return out
	}
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.HostPort == 0 {
				continue
			}
			out = append(out, hostport.Mapping{
				HostIP:        portBindHost(p.HostIP, bindHost),
				HostPort:      int(p.HostPort),
				ContainerPort: int(p.ContainerPort),
				Protocol:      string(p.Protocol),
			})
		}
	}
	return out
}

// hostPortMaps is the forward table for one pod. ip must be the address
// the host assigned to the VM. A guest-reported loopback, link-local, or
// other address is not forwarded, including from a liveness restart.
// An empty port hostIP listens on bindHost when that is a real interface
// address; otherwise the port's own hostIP is kept.
func hostPortMaps(pod *corev1.Pod, ip, bindHost string) []hostport.Mapping {
	var out []hostport.Mapping
	canonical, err := forwardableIP(ip)
	if err != nil || pod == nil {
		return out
	}
	for _, c := range pod.Spec.Containers {
		for _, p := range c.Ports {
			if p.HostPort == 0 {
				continue
			}
			out = append(out, hostport.Mapping{
				HostIP:        portBindHost(p.HostIP, bindHost),
				HostPort:      int(p.HostPort),
				PodIP:         canonical,
				ContainerPort: int(p.ContainerPort),
				Protocol:      string(p.Protocol),
			})
		}
	}
	return out
}

func portBindHost(explicit, bindHost string) string {
	if strings.TrimSpace(explicit) != "" {
		return explicit
	}
	return bindHost
}

// vmForwardIP is the address the host may publish or proxy to.
// assigned is the runtime's address (DHCP lease, ARP for the VM MAC, or
// the fake runtime's recorded IP). reported is the guest's NetInfo.
// The guest is believed only when it names that same address. Loopback,
// link-local, multicast, broadcast, and unspecified addresses are never
// a VM target. When the host has not assigned an address, reported is
// rejected and the result is empty. When the guest names something else,
// the result is still the host assignment and err explains the mismatch.
func vmForwardIP(reported, assigned string) (string, error) {
	assignedIP, aerr := forwardableIP(assigned)
	reported = strings.TrimSpace(reported)
	if aerr != nil {
		if reported == "" {
			return "", aerr
		}
		if _, rerr := forwardableIP(reported); rerr != nil {
			return "", fmt.Errorf("guest reported %s: %w", reported, rerr)
		}
		return "", fmt.Errorf("host has not assigned %s", reported)
	}
	if reported == "" {
		return assignedIP, nil
	}
	reportedIP, rerr := forwardableIP(reported)
	if rerr != nil {
		return assignedIP, fmt.Errorf("guest reported %s: %w", reported, rerr)
	}
	if reportedIP != assignedIP {
		return assignedIP, fmt.Errorf("guest reported %s, host assigned %s", reportedIP, assignedIP)
	}
	return assignedIP, nil
}

func forwardableIP(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("no VM address")
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return "", fmt.Errorf("%s is not an IP address", s)
	}
	if !ipUsableForVM(ip) {
		return "", fmt.Errorf("%s is not a VM address", ip)
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String(), nil
	}
	return ip.String(), nil
}

func ipUsableForVM(ip net.IP) bool {
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && v4[0] == 255 && v4[1] == 255 && v4[2] == 255 && v4[3] == 255 {
		return false
	}
	return true
}

// chooseBindHost picks the address an empty hostIP port listens on.
// A non-loopback local ListenAddress wins, then the node IP when that is
// also a local non-loopback address. Loopback is not used here: the kubelet
// default listen address is 127.0.0.1, and the host probe often reports lo
// first. Binding hostPort there would hide it from the rest of the machine.
// An explicit container hostIP of 127.0.0.1 is still honored by the caller.
// An empty result means the listener stays on all interfaces.
func chooseBindHost(listenAddr, nodeIP string, local func(net.IP) bool) string {
	if ip := bindableHost(listenAddr, local, false); ip != "" {
		return ip
	}
	return bindableHost(nodeIP, local, false)
}

func bindableHost(s string, local func(net.IP) bool, allowLoopback bool) string {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return ""
	}
	if ip.IsLoopback() && !allowLoopback {
		return ""
	}
	if local != nil && !local(ip) {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.String()
}

func (e *Engine) hostPortBindHost() string {
	if e == nil {
		return ""
	}
	return chooseBindHost(e.cfg.ListenAddress, e.hostIP, ipAssignedLocal)
}

func ipAssignedLocal(ip net.IP) bool {
	if ip == nil {
		return false
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP == nil {
			continue
		}
		if n.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// assignedVMIP is the address the host recorded for this VM. The guest's
// NetInfo is not a source. An empty status IP falls back to the ARP row
// for the VM MAC, which is the lease the host can see.
func assignedVMIP(machine runtime.Machine) string {
	if machine == nil {
		return ""
	}
	st := machine.Status()
	if ip := strings.TrimSpace(st.IP); ip != "" {
		return ip
	}
	if st.MAC == "" {
		return ""
	}
	found, err := netutil.IPForMAC(st.MAC)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(found)
}

// notePodIP stores the host-assigned address. A guest report that does not
// match is not stored. The previous address is left in place when the host
// still has nothing forwardable, so a later lie cannot retarget the proxy.
func (e *Engine) notePodIP(ctx context.Context, rec *podRecord, machine runtime.Machine, reported string) string {
	ip, err := vmForwardIP(reported, assignedVMIP(machine))
	// An empty report with no host assignment is the normal "no address yet"
	// case. Warn only when the guest named an address we will not use.
	if err != nil && e != nil && strings.TrimSpace(reported) != "" {
		e.events.Warn(ctx, event.ReasonFailed, "pod IP: "+err.Error())
	}
	if ip == "" || rec == nil {
		return ""
	}
	rec.mu.Lock()
	old := ""
	if rec.pod != nil {
		old = rec.pod.Status.PodIP
		rec.pod.Status.PodIP = ip
	}
	rec.mu.Unlock()
	if old != "" && old != ip && e != nil {
		e.events.Normal(ctx, event.ReasonPodIPChanged, old+" -> "+ip)
	}
	return ip
}

// syncHostPorts binds the proxy the first time the VM has a forwardable
// address, and retargets it after that. A restart that learns the address
// the host assigned still opens the listener; UpdateDest alone would not.
func (e *Engine) syncHostPorts(rec *podRecord, pod *corev1.Pod, ip string) error {
	if e == nil || pod == nil {
		return nil
	}
	bindHost := e.hostPortBindHost()
	on := false
	if rec != nil {
		rec.mu.Lock()
		if rec.hostPortBindSet {
			bindHost = rec.hostPortBind
		}
		on = rec.hostPortOn
		rec.mu.Unlock()
	}
	maps := hostPortMaps(pod, ip, bindHost)
	if len(maps) == 0 {
		return nil
	}
	key := Key(pod.Namespace, pod.Name)
	if on {
		e.hostports.UpdateDest(key, maps)
		return nil
	}
	if err := e.hostports.Bind(key, maps); err != nil {
		return err
	}
	if rec != nil {
		rec.mu.Lock()
		rec.hostPortOn = true
		rec.mu.Unlock()
	}
	return nil
}
