package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/hostport"

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

func probeTiming(p *corev1.Probe, defaultPeriod time.Duration) (initial, period time.Duration, failTh int32) {
	period = defaultPeriod
	if period <= 0 {
		period = 10 * time.Second
	}
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

func (e *Engine) watch(ctx context.Context, rec *podRecord) {
	interval := e.cfg.ProbeInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	startupOK := false
	var startupFails, liveFails int32
	e.refreshSidecarStatus(ctx, rec)
	if !e.tickProbes(ctx, rec, interval, &startupOK, &startupFails, &liveFails) {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			e.refreshSidecarStatus(ctx, rec)
			if !e.tickProbes(ctx, rec, interval, &startupOK, &startupFails, &liveFails) {
				return
			}
		}
	}
}

func (e *Engine) tickProbes(ctx context.Context, rec *podRecord, defaultPeriod time.Duration, startupOK *bool, startupFails, liveFails *int32) bool {
	rec.mu.Lock()
	pod := rec.pod.DeepCopy()
	cli := rec.agent
	phase := rec.phase
	var started time.Time
	if rec.startedAt != nil {
		started = rec.startedAt.Time
	}
	rec.mu.Unlock()
	if cli == nil || phase != corev1.PodRunning {
		return true
	}
	c0 := pod.Spec.Containers[0]
	now := time.Now()

	if !*startupOK {
		if req, ok := k8sProbe(c0.StartupProbe); ok {
			initial, _, failTh := probeTiming(c0.StartupProbe, defaultPeriod)
			if !started.IsZero() && now.Before(started.Add(initial)) {
				rec.mu.Lock()
				rec.ready = false
				rec.reason = "StartupProbe"
				rec.message = "initial delay"
				rec.mu.Unlock()
				return true
			}
			res, err := cli.Probe(ctx, req)
			if err != nil || !res.OK {
				*startupFails++
				rec.mu.Lock()
				rec.ready = false
				rec.reason = "StartupProbe"
				rec.message = probeMsg(res, err)
				rec.mu.Unlock()
				if *startupFails >= failTh {
					e.fail(rec, fmt.Errorf("startup probe failed: %s", probeMsg(res, err)))
					return false
				}
				return true
			}
		}
		*startupOK = true
	}

	var wg sync.WaitGroup

	if req, ok := k8sProbe(c0.ReadinessProbe); ok {
		initial, _, _ := probeTiming(c0.ReadinessProbe, defaultPeriod)
		if started.IsZero() || !now.Before(started.Add(initial)) {
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
		initial, _, failTh := probeTiming(c0.LivenessProbe, defaultPeriod)
		liveFailTh = failTh
		if started.IsZero() || !now.Before(started.Add(initial)) {
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
				e.fail(rec, fmt.Errorf("liveness restart: %w", rerr))
				return false
			}
			return true
		}
		e.fail(rec, fmt.Errorf("liveness probe failed: %s", probeMsg(res, err)))
		return false
	}
	*liveFails = 0
	return true
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
