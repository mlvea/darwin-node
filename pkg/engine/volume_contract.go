package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/volume"

	corev1 "k8s.io/api/core/v1"
)

// enforceVolumeContract applies emptyDir size limits and projected-token
// refresh. A false return means the pod was failed and the caller must stop.
func (e *Engine) enforceVolumeContract(ctx context.Context, rec *podRecord) bool {
	rec.mu.Lock()
	if rec.phase == corev1.PodFailed || rec.phase == corev1.PodSucceeded {
		rec.mu.Unlock()
		return false
	}
	limits := append([]volume.EmptyDirLimit(nil), rec.emptyDirs...)
	rec.mu.Unlock()
	for _, lim := range limits {
		n, err := volume.DirUsage(lim.Path)
		if err != nil {
			e.failWith(rec, event.ReasonEmptyDirExceeded, err)
			return false
		}
		if n > lim.Bytes {
			e.failWith(rec, event.ReasonEmptyDirExceeded, fmt.Errorf("emptyDir %q exceeds sizeLimit: %d bytes > %d", lim.Name, n, lim.Bytes))
			return false
		}
	}
	return e.refreshProjectedTokens(ctx, rec)
}

func (e *Engine) refreshProjectedTokens(ctx context.Context, rec *podRecord) bool {
	e.mu.RLock()
	minter := e.minter
	e.mu.RUnlock()
	if minter == nil {
		return true
	}

	rec.mu.Lock()
	if rec.phase != corev1.PodRunning {
		rec.mu.Unlock()
		return true
	}
	needCopy := rec.tokensNeedCopy
	due := !rec.refreshAt.IsZero() && !time.Now().Before(rec.refreshAt)
	pod := rec.pod.DeepCopy()
	root := rec.volRoot
	tokens := append([]volume.ProjectedToken(nil), rec.tokens...)
	rec.mu.Unlock()

	if needCopy {
		if err := e.rematerializeTokenVolumes(ctx, rec, tokens); err != nil {
			e.events.Warn(ctx, event.ReasonTokenRefreshFailed, err.Error())
		} else {
			rec.mu.Lock()
			rec.tokensNeedCopy = false
			rec.mu.Unlock()
		}
	}
	if !due {
		return true
	}

	minted, err := minter(ctx, pod)
	if err != nil || !tokensComplete(tokens, minted) {
		if err == nil {
			err = fmt.Errorf("projected service account token refresh returned an empty token")
		}
		return e.tokenRefreshFailed(ctx, rec, tokens, err)
	}
	if err := volume.WriteProjectedTokens(root, minted); err != nil {
		return e.tokenRefreshFailed(ctx, rec, tokens, err)
	}
	now := time.Now()
	rec.mu.Lock()
	rec.tokens = append([]volume.ProjectedToken(nil), minted...)
	rec.refreshAt = nextTokenRefresh(rec.tokens, now)
	rec.tokensNeedCopy = true
	rec.mu.Unlock()
	if err := e.rematerializeTokenVolumes(ctx, rec, minted); err != nil {
		e.events.Warn(ctx, event.ReasonTokenRefreshFailed, err.Error())
		return true
	}
	rec.mu.Lock()
	rec.tokensNeedCopy = false
	rec.mu.Unlock()
	e.events.Normal(ctx, event.ReasonTokenRefreshed, "projected service account tokens rewritten")
	return true
}

func (e *Engine) tokenRefreshFailed(ctx context.Context, rec *podRecord, tokens []volume.ProjectedToken, err error) bool {
	if tokenExpired(tokens, time.Now()) {
		e.failWith(rec, event.ReasonTokenExpired, fmt.Errorf("service account token expired: %w", err))
		return false
	}
	e.events.Warn(ctx, event.ReasonTokenRefreshFailed, err.Error())
	rec.mu.Lock()
	rec.refreshAt = time.Now().Add(e.probeInterval())
	rec.mu.Unlock()
	return true
}

func (e *Engine) probeInterval() time.Duration {
	if e.cfg.ProbeInterval > 0 {
		return e.cfg.ProbeInterval
	}
	return 10 * time.Second
}

func (e *Engine) rematerializeTokenVolumes(ctx context.Context, rec *podRecord, tokens []volume.ProjectedToken) error {
	rec.mu.Lock()
	cli := rec.agent
	root := rec.volRoot
	places := append([]volume.Placement(nil), rec.places...)
	rec.mu.Unlock()
	if cli == nil {
		return fmt.Errorf("guest agent not connected")
	}
	var vp []guest.VolumePlace
	for _, p := range places {
		if p.Mode != "copy" || !placeHoldsToken(root, p, tokens) {
			continue
		}
		vp = append(vp, guest.VolumePlace{Name: p.Name, GuestPath: p.GuestPath, ReadOnly: p.ReadOnly, Mode: p.Mode})
	}
	if len(vp) == 0 {
		return nil
	}
	_, err := cli.Materialize(ctx, guest.MaterializeReq{Volumes: vp})
	return err
}

func placeHoldsToken(root string, p volume.Placement, tokens []volume.ProjectedToken) bool {
	host := filepath.Clean(p.HostPath)
	for _, t := range tokens {
		dir := filepath.Clean(filepath.Join(root, t.Volume))
		if host == dir || strings.HasPrefix(host, dir+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func nextTokenRefresh(tokens []volume.ProjectedToken, now time.Time) time.Time {
	var earliest time.Time
	for _, t := range tokens {
		if t.ExpiresAt.IsZero() {
			continue
		}
		if earliest.IsZero() || t.ExpiresAt.Before(earliest) {
			earliest = t.ExpiresAt
		}
	}
	if earliest.IsZero() {
		return time.Time{}
	}
	if !earliest.After(now) {
		return now
	}
	margin := earliest.Sub(now) / 2
	return earliest.Add(-margin)
}

func tokenExpired(tokens []volume.ProjectedToken, now time.Time) bool {
	for _, t := range tokens {
		if !t.ExpiresAt.IsZero() && !t.ExpiresAt.After(now) {
			return true
		}
	}
	return false
}

func tokensComplete(prev, next []volume.ProjectedToken) bool {
	if len(prev) == 0 {
		return false
	}
	have := map[string]string{}
	for _, t := range next {
		have[tokenKey(t)] = t.Token
	}
	for _, t := range prev {
		tok, ok := have[tokenKey(t)]
		if !ok || strings.TrimSpace(tok) == "" {
			return false
		}
	}
	return true
}

func tokenKey(t volume.ProjectedToken) string {
	p := t.Path
	if p == "" {
		p = "token"
	}
	return t.Volume + "\x00" + p
}
