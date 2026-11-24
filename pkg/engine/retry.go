package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime"

	corev1 "k8s.io/api/core/v1"
)

// Start retries. The delay is bounded. The attempt count is not: a pull,
// boot, or dial keeps going until it works, the error is permanent, or the
// pod context is cancelled (delete or shutdown). The VM slot acquired in
// Create stays held the whole time. Releasing it would admit another VM on
// a 2-VM host while this pod is about to retry, and failing the pod would
// make a ReplicaSet create the next Failed pod. The pod stays Pending with
// reason BackOff.
const (
	startRetryInitial = time.Second
	startRetryMax     = 30 * time.Second
)

// permanentStartError is implemented by errors that must not be retried.
// Image size limits and layer-cycle limits should implement it. Text
// matching below covers those phrases when the error comes from another
// package that does not.
type permanentStartError interface {
	Permanent() bool
}

func (e *Engine) retryTransient(ctx context.Context, rec *podRecord, what string, fn func() error) error {
	delay := startRetryInitial
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isPermanentStartError(err) {
			return err
		}
		wait := jitterBackoff(delay)
		e.noteBackoff(rec, what, wait, err)
		if werr := e.sleepBackoff(ctx, wait); werr != nil {
			return werr
		}
		delay = nextBackoff(delay)
	}
}

func (e *Engine) noteBackoff(rec *podRecord, what string, wait time.Duration, err error) {
	msg := fmt.Sprintf("back-off %s retrying %s: %v", wait.Round(time.Millisecond), what, err)
	if rec != nil {
		rec.mu.Lock()
		if rec.phase != corev1.PodFailed && rec.phase != corev1.PodSucceeded {
			rec.reason = event.ReasonBackOff
			rec.message = msg
		}
		rec.mu.Unlock()
	}
	e.events.Warn(context.Background(), event.ReasonBackOff, msg)
}

func (e *Engine) sleepBackoff(ctx context.Context, d time.Duration) error {
	timer := e.newProbeTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C():
		return nil
	}
}

// jitterBackoff returns a wait in [d/2, d]. Advance(d) on the test clock
// always fires it, and the wait is never zero when d is positive.
func jitterBackoff(d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	span := int64(d) / 2
	if span <= 0 {
		return d
	}
	return d/2 + time.Duration(rand.Int64N(span+1))
}

func nextBackoff(d time.Duration) time.Duration {
	n := d * 2
	if n <= 0 || n > startRetryMax {
		return startRetryMax
	}
	return n
}

func isPermanentStartError(err error) bool {
	if err == nil {
		return false
	}
	var perm permanentStartError
	if errors.As(err, &perm) && perm.Permanent() {
		return true
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "unauthorized"),
		strings.Contains(msg, "authentication required"),
		strings.Contains(msg, "auth denied"),
		strings.Contains(msg, "forbidden"),
		strings.Contains(msg, "invalid image"),
		strings.Contains(msg, "invalid reference"),
		strings.Contains(msg, "parse image ref"),
		strings.Contains(msg, "not allowed in cluster mode"),
		strings.Contains(msg, "image config missing"),
		strings.Contains(msg, "no disk.img"),
		strings.Contains(msg, "negative size"):
		return true
	}
	// Declared, compressed, and decompressed caps. "exceeds limit" covers
	// "compressed blob exceeds limit", which does not contain "size".
	// A pull deadline ("context deadline exceeded") and a read stall do
	// not match: those are transient and are retried.
	if strings.Contains(msg, "exceeds limit") || strings.Contains(msg, "exceed limit") || strings.Contains(msg, "exceeded limit") {
		return true
	}
	if strings.Contains(msg, "size") && (strings.Contains(msg, "limit") || strings.Contains(msg, "exceed") || strings.Contains(msg, "too large")) {
		return true
	}
	// Delta cycles. "lifecycle" contains "cycle", so the word alone is not
	// a match. "cycles through" and "is itself a delta" are.
	if strings.Contains(msg, "cycles through") || strings.Contains(msg, "is itself a delta") || strings.Contains(msg, "is the image itself") {
		return true
	}
	if strings.Contains(msg, "cycle") && (strings.Contains(msg, "limit") || strings.Contains(msg, "exceed")) {
		return true
	}
	if strings.Contains(msg, "cyclic") {
		return true
	}
	return false
}

func (e *Engine) stopMachineKeepSlot(rec *podRecord) {
	if rec == nil {
		return
	}
	rec.mu.Lock()
	m := rec.machine
	rec.machine = nil
	rec.mu.Unlock()
	if m != nil {
		_ = m.Stop(context.Background(), 0)
	}
}

func (e *Engine) agentDialBudget() time.Duration {
	if e != nil && e.cfg.AgentReadyTimeout > 0 {
		return e.cfg.AgentReadyTimeout
	}
	return 2 * time.Minute
}

// dialGuest dials the guest agent, then TCP if vsock did not connect, and
// waits for Ready. Each attempt's budget is AgentReadyTimeout. A refused
// dial or a guest that is not ready yet is retried. The client returned
// is the one that became ready.
func (e *Engine) dialGuest(ctx context.Context, rec *podRecord, machine runtime.Machine, token string) (*guest.Client, error) {
	var cli *guest.Client
	err := e.retryTransient(ctx, rec, "guest agent dial", func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		budget := e.agentDialBudget()
		dialCtx, cancel := context.WithTimeout(ctx, budget)
		c, derr := machine.DialAgent(dialCtx)
		cancel()
		if derr != nil && ctx.Err() == nil {
			fbCtx, fbCancel := context.WithTimeout(ctx, tcpFallbackTimeout)
			c2, ferr := e.dialFallback(fbCtx, token, machine)
			fbCancel()
			if ferr == nil {
				if c != nil {
					_ = c.Close()
				}
				c, derr = c2, nil
			}
		}
		if derr != nil {
			if c != nil {
				_ = c.Close()
			}
			return fmt.Errorf("guest agent: %w", derr)
		}
		readyCtx, readyCancel := context.WithTimeout(ctx, budget)
		ready, rerr := c.Ready(readyCtx)
		readyCancel()
		if rerr != nil || !ready.Ready {
			_ = c.Close()
			if rerr == nil {
				rerr = errors.New("not ready")
			}
			return fmt.Errorf("guest agent not ready: %v %w", ready, rerr)
		}
		cli = c
		return nil
	})
	return cli, err
}
