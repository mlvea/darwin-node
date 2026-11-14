package engine

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

// sidecarStartState is the startup probe result for one sidecar generation.
// at is the container start time the result belongs to. A new start time
// means the container was restarted and the probe has to pass again.
type sidecarStartState struct {
	at    time.Time
	done  bool
	fails int32
	msg   string
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

// tickSidecarStartup runs startup probes for hybrid sidecars. The VM's
// probes stay on the guest agent; these run against the host container.
// Past the failure threshold the pod fails and the VM slot is released,
// the same as the main container's startup probe.
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
		rec.mu.Lock()
		if rec.sidecarStart == nil {
			rec.sidecarStart = map[string]*sidecarStartState{}
		}
		cur := rec.sidecarStart[c.Name]
		if cur == nil || !cur.at.Equal(s.StartedAt) {
			cur = &sidecarStartState{at: s.StartedAt}
			rec.sidecarStart[c.Name] = cur
		}
		if cur.done {
			rec.mu.Unlock()
			continue
		}
		rec.mu.Unlock()

		initial, _, failTh := probeTiming(c.StartupProbe, e.cfg.ProbeInterval)
		if !s.StartedAt.IsZero() && time.Since(s.StartedAt) < initial {
			rec.mu.Lock()
			cur.msg = "initial delay"
			rec.mu.Unlock()
			continue
		}
		if ctx.Err() != nil {
			return true
		}
		okProbe, msg := runSidecarProbe(ctx, e.sidecar, pod.Namespace, pod.Name, c.Name, c.StartupProbe)
		if ctx.Err() != nil {
			return true
		}
		rec.mu.Lock()
		if okProbe {
			cur.done = true
			cur.fails = 0
			cur.msg = ""
			rec.mu.Unlock()
			continue
		}
		cur.fails++
		fails := cur.fails
		cur.msg = msg
		rec.mu.Unlock()
		if fails >= failTh {
			e.fail(rec, fmt.Errorf("sidecar %s startup probe failed: %s", c.Name, msg))
			return false
		}
	}
	return true
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
