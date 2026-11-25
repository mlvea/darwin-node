package engine

import (
	"context"

	"github.com/mlvea/darwin-node/pkg/guest"
)

// DebugPod is a JSON-friendly engine view for the visual debugger.
type DebugPod struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	UID          string `json:"uid"`
	Phase        string `json:"phase"`
	Reason       string `json:"reason"`
	Message      string `json:"message"`
	PodIP        string `json:"podIP"`
	Ready        bool   `json:"ready"`
	AgentOK      bool   `json:"agentOK"`
	RestartCount int32  `json:"restartCount"`
	VMState      string `json:"vmState"`
}

func (e *Engine) DebugSnapshot() (used, max int, pods []DebugPod) {
	used = e.slots.Used()
	max = e.slots.Max()
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, rec := range e.pods {
		rec.mu.Lock()
		dp := DebugPod{
			Namespace:    rec.pod.Namespace,
			Name:         rec.pod.Name,
			UID:          string(rec.pod.UID),
			Phase:        string(rec.phase),
			Reason:       rec.reason,
			Message:      rec.message,
			PodIP:        rec.pod.Status.PodIP,
			Ready:        rec.ready,
			AgentOK:      rec.agentOK,
			RestartCount: rec.restartCount,
		}
		if rec.machine != nil {
			st := rec.machine.Status()
			dp.VMState = string(st.State)
			if canonical, ferr := forwardableIP(st.IP); ferr == nil {
				dp.PodIP = canonical
			}
		}
		if canonical, ferr := forwardableIP(dp.PodIP); ferr == nil {
			dp.PodIP = canonical
		} else {
			dp.PodIP = ""
		}
		rec.mu.Unlock()
		pods = append(pods, dp)
	}
	return used, max, pods
}

func (e *Engine) PodMetrics(ctx context.Context, ns, name string) (guest.MetricsRes, bool) {
	e.mu.RLock()
	rec, ok := e.pods[Key(ns, name)]
	e.mu.RUnlock()
	if !ok {
		return guest.MetricsRes{}, false
	}
	rec.mu.Lock()
	cli := rec.agent
	rec.mu.Unlock()
	// An older agent advertises no capabilities. Publishing its zeros
	// would look like an idle VM. Allocation fallback is the honest
	// number until the agent says it can measure.
	if cli == nil || !cli.HasCapability(guest.CapMetrics) {
		return guest.MetricsRes{}, false
	}
	m, err := cli.Metrics(ctx)
	if err != nil {
		return guest.MetricsRes{}, false
	}
	return m, true
}
