package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
	flights.start(ctx, key, started, func(pctx context.Context) (bool, string) {
		return runSidecarProbe(pctx, e.sidecar, ns, podName, cName, p)
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

func runSidecarProbe(ctx context.Context, rt sidecar.Runtime, ns, podName, container string, p *corev1.Probe) (ok bool, msg string) {
	req, supported := k8sProbe(p)
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
		if pctx.Err() != nil {
			return false, probeIOMessage(pctx.Err())
		}
		if err != nil {
			return false, clipProbe(err.Error())
		}
		return true, ""
	case guest.ProbeHTTP, guest.ProbeTCP:
		host, port, err := sidecarProbeTarget(pctx, rt, ns, podName, container, p)
		if err != nil {
			return false, clipProbe(err.Error())
		}
		if req.Type == guest.ProbeHTTP {
			return sidecarHTTPProbe(pctx, p, host, port)
		}
		return sidecarTCPProbe(pctx, host, port)
	default:
		return false, "unknown probe type"
	}
}

// sidecarProbeTarget is the container address the probe may dial.
// An empty host, localhost, or a loopback address means the sidecar.
// Any other host is refused, and the dial never happens.
// The message is published on the pod and on events, so it carries no body.
func sidecarProbeTarget(ctx context.Context, rt sidecar.Runtime, ns, podName, container string, p *corev1.Probe) (string, int, error) {
	specHost, port, err := probeSpecHostPort(p)
	if err != nil {
		return "", 0, err
	}
	ep, ok := rt.(sidecar.Endpoint)
	if !ok {
		return "", 0, fmt.Errorf("sidecar probe: runtime has no container address")
	}
	ip, err := ep.Endpoint(ctx, ns, podName, container)
	if err != nil || strings.TrimSpace(ip) == "" {
		return "", 0, fmt.Errorf("sidecar probe: runtime has no container address")
	}
	got, ok := sidecar.RoutableIP(ip)
	if !ok {
		return "", 0, fmt.Errorf("sidecar probe: container address is not routable")
	}
	host := got
	if !probeHostUsesContainer(specHost) {
		if !sameProbeHost(specHost, got) {
			return "", 0, fmt.Errorf("sidecar probe: host %q is not the sidecar", specHost)
		}
	}
	if _, ok := sidecar.RoutableIP(host); !ok {
		return "", 0, fmt.Errorf("sidecar probe: refusing host loopback")
	}
	return host, port, nil
}

func probeSpecHostPort(p *corev1.Probe) (string, int, error) {
	if p == nil {
		return "", 0, fmt.Errorf("sidecar probe: no probe")
	}
	var host string
	var port int
	switch {
	case p.HTTPGet != nil:
		host = p.HTTPGet.Host
		port = p.HTTPGet.Port.IntValue()
	case p.TCPSocket != nil:
		host = p.TCPSocket.Host
		port = p.TCPSocket.Port.IntValue()
	default:
		return "", 0, fmt.Errorf("sidecar probe: no host")
	}
	if port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("sidecar probe: port %d is invalid", port)
	}
	return host, port, nil
}

func probeHostUsesContainer(host string) bool {
	h := strings.TrimSpace(host)
	if h == "" || strings.EqualFold(h, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

func sameProbeHost(spec, container string) bool {
	a := net.ParseIP(strings.Trim(strings.TrimSpace(spec), "[]"))
	b := net.ParseIP(strings.Trim(strings.TrimSpace(container), "[]"))
	if a != nil && b != nil {
		return a.Equal(b)
	}
	return strings.EqualFold(strings.TrimSpace(spec), strings.TrimSpace(container))
}

func sidecarHTTPProbe(ctx context.Context, p *corev1.Probe, host string, port int) (bool, string) {
	if _, ok := sidecar.RoutableIP(host); !ok {
		return false, "sidecar probe: refusing host loopback"
	}
	raw, err := sidecarProbeURL(p, host, port)
	if err != nil {
		return false, clipProbe(err.Error())
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return false, "probe dial failed"
	}
	if p != nil && p.HTTPGet != nil {
		for _, h := range p.HTTPGet.HTTPHeaders {
			if h.Name == "" || strings.EqualFold(h.Name, "Host") {
				continue
			}
			httpReq.Header.Set(h.Name, h.Value)
		}
	}
	resp, err := sidecarProbeClient().Do(httpReq)
	if err != nil {
		return false, probeIOMessage(err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 400 {
		return false, fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return true, ""
}

func sidecarProbeURL(p *corev1.Probe, host string, port int) (string, error) {
	scheme := "http"
	path := "/"
	if p != nil && p.HTTPGet != nil {
		if p.HTTPGet.Scheme != "" {
			scheme = strings.ToLower(string(p.HTTPGet.Scheme))
		}
		if p.HTTPGet.Path != "" {
			path = p.HTTPGet.Path
		}
	}
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("sidecar probe: scheme %q is not supported", scheme)
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u := &url.URL{
		Scheme: scheme,
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
		Path:   path,
	}
	return u.String(), nil
}

func sidecarProbeClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       (&net.Dialer{}).DialContext,
			DisableKeepAlives: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func sidecarTCPProbe(ctx context.Context, host string, port int) (bool, string) {
	if _, ok := sidecar.RoutableIP(host); !ok {
		return false, "sidecar probe: refusing host loopback"
	}
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return false, probeIOMessage(err)
	}
	_ = c.Close()
	return true, ""
}

func probeIOMessage(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.Canceled) {
		return "probe canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "probe timed out"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "probe timed out"
	}
	return "probe dial failed"
}

func clipProbe(msg string) string {
	msg = strings.ReplaceAll(msg, "\n", " ")
	if len(msg) > 160 {
		return msg[:160]
	}
	return msg
}
