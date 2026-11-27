package observability

import (
	"context"

	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/supervisor"
)

// supervisorRecoverer is the optional startup hook. The supervised
// adapter implements it. Fake and vz do not.
type supervisorRecoverer interface {
	Recover(context.Context, []supervisor.PodPresence) ([]runtime.Machine, error)
}

// Recover forwards the supervised adapter's startup adopt. run wraps
// every runtime in Meter before the engine is built, and a type assert
// on the outer runtime would otherwise miss the hook. A runtime with
// no Recover returns no machines.
func (r *meteredRuntime) Recover(ctx context.Context, pods []supervisor.PodPresence) ([]runtime.Machine, error) {
	if r == nil {
		return nil, nil
	}
	inner, ok := r.inner.(supervisorRecoverer)
	if !ok || inner == nil {
		return nil, nil
	}
	machines, err := inner.Recover(ctx, pods)
	if len(machines) == 0 {
		return nil, err
	}
	out := make([]runtime.Machine, 0, len(machines))
	for _, m := range machines {
		if m == nil {
			continue
		}
		out = append(out, r.wrap(m))
	}
	return out, err
}

func (r *meteredRuntime) wrap(m runtime.Machine) runtime.Machine {
	base := meteredMachine{inner: m, metrics: r.metrics}
	if _, ok := m.(runtime.Consoler); ok {
		return &meteredConsole{base}
	}
	return &base
}

// SetAgentToken forwards the guest handshake token. Recover does not
// carry it, and the engine sets it before redialing. The recovered
// machine is this wrapper, so the method has to reach the adapter.
func (m *meteredMachine) SetAgentToken(token string) {
	if m == nil || m.inner == nil {
		return
	}
	setter, ok := m.inner.(interface{ SetAgentToken(string) })
	if !ok {
		return
	}
	setter.SetAgentToken(token)
}
