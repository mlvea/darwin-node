package engine

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

// sidecarStartState is the probe result for one sidecar generation.
// at is the container start time the result belongs to. A new start time
// means the container was restarted and every probe starts again.
// last, liveLast, and readyLast are when that probe last started. Zero
// means the next sample is due once the initial delay has elapsed.
// The streaks are the same probeStreak the VM probes use.
type sidecarStartState struct {
	at        time.Time
	startup   probeStreak
	last      time.Time
	liveness  probeStreak
	liveLast  time.Time
	readiness probeStreak
	readyLast time.Time
}

func sidecarStartupPassed(st *sidecarStartState, s sidecar.Status) bool {
	if st == nil || !st.startup.passed {
		return false
	}
	if !s.StartedAt.IsZero() && !st.at.Equal(s.StartedAt) {
		return false
	}
	return true
}

func sidecarReadyPassed(st *sidecarStartState, s sidecar.Status) bool {
	if st == nil || !st.readiness.passed {
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
		if cur := rec.sidecarStart[c.Name]; cur != nil && cur.startup.msg != "" {
			msg = cur.startup.msg
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
		if cur := rec.sidecarStart[c.Name]; cur != nil && cur.readiness.msg != "" {
			msg = cur.readiness.msg
		}
		return fmt.Sprintf("sidecar %s %s", c.Name, msg), true
	}
	return "", false
}

// sideKind identifies which probe a flight belongs to.
const (
	sideKindStartup   = "startup"
	sideKindLiveness  = "liveness"
	sideKindReadiness = "readiness"
)

// sideResult is one finished sidecar probe. The watch applies it; the
// probe goroutine only runs the I/O.
type sideResult struct {
	ok  bool
	msg string
}

// sideFlight is one in-flight sidecar probe. The map is touched only on
// the watch goroutine. The probe goroutine writes result and wake.
type sideFlight struct {
	started time.Time
	cancel  context.CancelFunc
	result  chan sideResult
}

// sideFlights tracks sidecar probes that must not block VM probe samples.
// wake is buffered so a finished probe runs its result on the watch
// without waiting out the next period. stop joins every goroutine.
type sideFlights struct {
	m    map[string]*sideFlight
	wg   sync.WaitGroup
	wake chan struct{}
}

func newSideFlights() *sideFlights {
	return &sideFlights{m: map[string]*sideFlight{}, wake: make(chan struct{}, 1)}
}

func (f *sideFlights) inflight(key string) bool {
	_, ok := f.m[key]
	return ok
}

func (f *sideFlights) start(parent context.Context, key string, started time.Time, run func(context.Context) (bool, string)) {
	if f == nil || f.inflight(key) {
		return
	}
	pctx, cancel := context.WithCancel(parent)
	fl := &sideFlight{started: started, cancel: cancel, result: make(chan sideResult, 1)}
	f.m[key] = fl
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		ok, msg := run(pctx)
		fl.result <- sideResult{ok: ok, msg: msg}
		select {
		case f.wake <- struct{}{}:
		default:
		}
	}()
}

func (f *sideFlights) take(key string, started time.Time) (sideResult, bool) {
	fl := f.m[key]
	if fl == nil {
		return sideResult{}, false
	}
	if !fl.started.Equal(started) {
		fl.cancel()
		delete(f.m, key)
		return sideResult{}, false
	}
	select {
	case res := <-fl.result:
		fl.cancel()
		delete(f.m, key)
		return res, true
	default:
		return sideResult{}, false
	}
}

func (f *sideFlights) drop(key string) {
	fl := f.m[key]
	if fl == nil {
		return
	}
	fl.cancel()
	delete(f.m, key)
}

func (f *sideFlights) stop() {
	if f == nil {
		return
	}
	for _, fl := range f.m {
		fl.cancel()
	}
	f.wg.Wait()
}

func sideProbeKey(name, kind string) string { return name + "/" + kind }

// settleSidecarProbes applies sidecar probe results that have already
// finished. The probe I/O itself is not run here, so a slow sample cannot
// hold the VM probes. A kill returns true for the watch and skips the rest
// of this pass; the next wake sees the restarted container.
func (e *Engine) settleSidecarProbes(ctx context.Context, rec *podRecord, flights *sideFlights) bool {
	pod, byName, ok := e.sidecarProbePod(rec)
	if !ok {
		return true
	}
	for _, c := range pod.Spec.Containers[1:] {
		s, found := byName[c.Name]
		for _, spec := range []struct {
			kind  string
			probe *corev1.Probe
		}{
			{sideKindStartup, c.StartupProbe},
			{sideKindLiveness, c.LivenessProbe},
			{sideKindReadiness, c.ReadinessProbe},
		} {
			if spec.probe == nil {
				continue
			}
			key := sideProbeKey(c.Name, spec.kind)
			if !found || s.State != "running" {
				flights.drop(key)
				continue
			}
			res, ready := flights.take(key, s.StartedAt)
			if !ready {
				continue
			}
			if ctx.Err() != nil || !podActive(rec) {
				return false
			}
			cont, stopWatch := e.applySidecarResult(ctx, rec, pod, c.Name, s.StartedAt, spec.kind, spec.probe, res)
			if stopWatch {
				return false
			}
			if !cont {
				return true
			}
		}
	}
	return true
}

func (e *Engine) applySidecarResult(ctx context.Context, rec *podRecord, pod *corev1.Pod, name string, started time.Time, kind string, probe *corev1.Probe, res sideResult) (cont, stopWatch bool) {
	_, _, failTh := probeTiming(probe)
	rec.mu.Lock()
	cur := sidecarProbeStateLocked(rec, name, started)
	var failed bool
	switch kind {
	case sideKindStartup:
		_, failed = cur.startup.observe(res.ok, res.msg, probeSuccessThreshold(probe), failTh)
	case sideKindLiveness:
		_, failed = cur.liveness.observe(res.ok, res.msg, 1, failTh)
	default:
		cur.readiness.observe(res.ok, res.msg, probeSuccessThreshold(probe), failTh)
	}
	rec.mu.Unlock()
	if kind == sideKindReadiness || !failed {
		return true, false
	}
	if !e.stopSidecarAfterProbe(ctx, rec, pod, name, kind, res.msg) {
		return false, true
	}
	return false, false
}

// launchSidecarProbes starts due sidecar probes without waiting for them.
// A probe that is already in flight is left alone, so a period that elapses
// while a sample is running does not pile up goroutines.
func (e *Engine) launchSidecarProbes(ctx context.Context, rec *podRecord, flights *sideFlights) {
	pod, byName, ok := e.sidecarProbePod(rec)
	if !ok {
		return
	}
	now := e.now()
	for _, c := range pod.Spec.Containers[1:] {
		s, found := byName[c.Name]
		if !found || s.State != "running" {
			continue
		}
		if hasStartupProbe(c) {
			e.launchSideProbe(ctx, rec, flights, pod, c, s, now, sideKindStartup, c.StartupProbe)
		}
		if sidecarStartupOpen(rec, c, s) {
			continue
		}
		if hasLivenessProbe(c) {
			e.launchSideProbe(ctx, rec, flights, pod, c, s, now, sideKindLiveness, c.LivenessProbe)
		}
		if hasReadinessProbe(c) {
			e.launchSideProbe(ctx, rec, flights, pod, c, s, now, sideKindReadiness, c.ReadinessProbe)
		}
	}
}

func (e *Engine) launchSideProbe(ctx context.Context, rec *podRecord, flights *sideFlights, pod *corev1.Pod, c corev1.Container, s sidecar.Status, now time.Time, kind string, probe *corev1.Probe) {
	if probe == nil {
		return
	}
	key := sideProbeKey(c.Name, kind)
	if flights.inflight(key) {
		return
	}
	initial, period, _ := probeTiming(probe)
	rec.mu.Lock()
	cur := sidecarProbeStateLocked(rec, c.Name, s.StartedAt)
	if kind == sideKindStartup && cur.startup.passed {
		rec.mu.Unlock()
		return
	}
	var last time.Time
	switch kind {
	case sideKindStartup:
		last = cur.last
	case sideKindLiveness:
		last = cur.liveLast
	default:
		last = cur.readyLast
	}
	due, _ := probeDue(now, s.StartedAt, last, initial, period)
	if !due {
		if initial > 0 && !s.StartedAt.IsZero() && now.Before(s.StartedAt.Add(initial)) {
			switch kind {
			case sideKindStartup:
				cur.startup.msg = "initial delay"
			case sideKindReadiness:
				cur.readiness.msg = "initial delay"
			}
		}
		rec.mu.Unlock()
		return
	}
	switch kind {
	case sideKindStartup:
		cur.last = now
	case sideKindLiveness:
		cur.liveLast = now
	default:
		cur.readyLast = now
	}
	rec.mu.Unlock()

	ns, podName, cName := pod.Namespace, pod.Name, c.Name
	p := probe
	started := s.StartedAt
	ports := append([]corev1.ContainerPort(nil), c.Ports...)
	flights.start(ctx, key, started, func(pctx context.Context) (bool, string) {
		return runSidecarProbe(pctx, e.sidecar, ns, podName, cName, p, ports)
	})
}

// stopSidecarAfterProbe kills the container and applies the restart policy.
// False means the watch must stop (the pod failed, or the context is done).
func (e *Engine) stopSidecarAfterProbe(ctx context.Context, rec *podRecord, pod *corev1.Pod, name, kind, msg string) bool {
	if ctx.Err() != nil || !podActive(rec) {
		return false
	}
	err := e.stopSidecarContainer(ctx, pod.Namespace, pod.Name, name)
	if ctx.Err() != nil || !podActive(rec) {
		return false
	}
	label := "liveness"
	if kind == sideKindStartup {
		label = "startup"
	}
	if err != nil {
		e.fail(rec, fmt.Errorf("sidecar %s %s probe failed: %s", name, label, msg))
		return false
	}
	e.events.Warn(ctx, event.ReasonProbeFailed, fmt.Sprintf("sidecar %s %s probe failed: %s", name, label, msg))
	e.refreshSidecarStatus(ctx, rec)
	return e.noteSidecarExit(rec) && e.restartExitedSidecars(ctx, rec)
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

func (e *Engine) stopSidecarContainer(ctx context.Context, ns, name, container string) error {
	stopper, ok := e.sidecar.(sidecar.ContainerStopper)
	if !ok {
		return fmt.Errorf("sidecar runtime cannot stop containers")
	}
	return stopper.StopContainer(ctx, ns, name, container)
}

func runSidecarProbe(ctx context.Context, rt sidecar.Runtime, ns, podName, container string, p *corev1.Probe, ports []corev1.ContainerPort) (ok bool, msg string) {
	req, supported := k8sProbe(p, ports)
	if !supported {
		return true, ""
	}
	start := time.Now()
	defer func() {
		var err error
		if !ok {
			text := msg
			if text == "" {
				text = "probe failed"
			}
			err = fmt.Errorf("%s", text)
		}
		observeProbe("sidecar", start, err)
	}()
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
