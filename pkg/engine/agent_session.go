package engine

import (
	"context"
	"time"

	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// guestAgentLostReason is the PodReady reason while redial is past its
// budget. Started stays true: the VM is up, the session is not.
const guestAgentLostReason = "GuestAgentDisconnected"

// Redial pacing. Tests shorten these. There is no process flag.
var (
	agentRedialBackoff = time.Second
	agentRedialMax     = 5 * time.Second
	agentRedialBudget  = 30 * time.Second
)

// dialAgent is the vsock dial used on the first connect and on a later
// redial. A parent context with no deadline gets AgentReadyTimeout (same
// budget as dialGuest).
func (e *Engine) dialAgent(ctx context.Context, machine runtime.Machine) (*guest.Client, error) {
	dialCtx := ctx
	cancelDial := func() {}
	if _, ok := ctx.Deadline(); !ok {
		dialCtx, cancelDial = context.WithTimeout(ctx, e.agentDialBudget())
	}
	defer cancelDial()
	return machine.DialAgent(dialCtx)
}

func (e *Engine) agentSession(rec *podRecord) *guest.Client {
	if rec == nil {
		return nil
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.agent
}

// recoverAgent dials again after lost's session ends. It keeps trying
// while the pod is running. Past agentRedialBudget of consecutive failure
// the pod is not Ready. A successful dial clears that reason. The VM is
// not restarted and the workload is not sent again: this is the same guest.
// False means the watch should return.
func (e *Engine) recoverAgent(ctx context.Context, rec *podRecord, lost *guest.Client) bool {
	if lost == nil || rec == nil {
		return ctx.Err() == nil
	}
	rec.mu.Lock()
	cur := rec.agent
	machine := rec.machine
	rec.mu.Unlock()
	if cur != lost {
		return ctx.Err() == nil && podActive(rec)
	}
	if err := ctx.Err(); err != nil || !podActive(rec) {
		return false
	}
	if machine == nil || machine.Status().State != types.VMRunning {
		return true
	}
	_ = lost.Close()

	var failedSince time.Time
	backoff := agentRedialBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	for {
		if err := ctx.Err(); err != nil || !podActive(rec) {
			return false
		}
		rec.mu.Lock()
		replaced := rec.agent != lost && rec.agent != nil
		rec.mu.Unlock()
		if replaced {
			return true
		}
		cli, err := e.dialAgent(ctx, machine)
		if err == nil {
			if e.installAgent(rec, lost, cli) {
				e.watchGuestExit(rec, cli)
				e.clearAgentLost(rec)
				e.events.Normal(ctx, event.ReasonGuestAgentRedial, "guest agent reconnected")
				return true
			}
			_ = cli.Close()
			return ctx.Err() == nil && podActive(rec)
		}
		if failedSince.IsZero() {
			failedSince = time.Now()
		}
		if time.Since(failedSince) >= agentRedialBudget {
			e.markAgentLost(rec)
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
		if !podActive(rec) {
			return false
		}
		if backoff < agentRedialMax {
			backoff *= 2
			if backoff > agentRedialMax {
				backoff = agentRedialMax
			}
		}
	}
}

func (e *Engine) installAgent(rec *podRecord, prev, cli *guest.Client) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.phase != corev1.PodPending && rec.phase != corev1.PodRunning {
		return false
	}
	if rec.agent != prev && rec.agent != nil {
		return false
	}
	rec.agent = cli
	rec.agentOK = true
	return true
}

func (e *Engine) markAgentLost(rec *podRecord) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.phase != corev1.PodRunning {
		return
	}
	// Leave agentOK set. Clearing it would drop Started while the VM is
	// still the container that passed its startup probe.
	rec.ready = false
	rec.reason = guestAgentLostReason
	rec.message = "guest agent session lost"
}

func (e *Engine) clearAgentLost(rec *podRecord) {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.reason != guestAgentLostReason {
		return
	}
	rec.message = ""
	if rec.pod != nil && len(rec.pod.Spec.Containers) > 0 {
		if _, has := k8sProbe(rec.pod.Spec.Containers[0].ReadinessProbe, rec.pod.Spec.Containers[0].Ports); has {
			rec.ready = false
			rec.reason = "ReadinessProbe"
			rec.message = "guest agent reconnected"
			return
		}
	}
	rec.reason = ""
	rec.ready = rec.startupDone
}
