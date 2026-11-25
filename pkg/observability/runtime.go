package observability

import (
	"context"
	"io"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/types"
)

// Meter wraps rt so each Machine.Start is a boot sample. Create is not
// a boot. A machine that implements Consoler keeps that method; other
// machines stay non-Consoler so a type assert still means a console exists.
func Meter(rt runtime.Runtime, m *Metrics) runtime.Runtime {
	if rt == nil || m == nil {
		return rt
	}
	return &meteredRuntime{inner: rt, metrics: m}
}

type meteredRuntime struct {
	inner   runtime.Runtime
	metrics *Metrics
}

func (r *meteredRuntime) Name() types.RuntimeName { return r.inner.Name() }

func (r *meteredRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.inner.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	base := meteredMachine{inner: m, metrics: r.metrics}
	if _, ok := m.(runtime.Consoler); ok {
		return &meteredConsole{base}, nil
	}
	return &base, nil
}

type meteredMachine struct {
	inner   runtime.Machine
	metrics *Metrics
}

func (m *meteredMachine) ID() types.MachineID { return m.inner.ID() }

func (m *meteredMachine) Start(ctx context.Context) error {
	start := time.Now()
	err := m.inner.Start(ctx)
	m.metrics.ObserveBoot(time.Since(start), err)
	return err
}

func (m *meteredMachine) Stop(ctx context.Context, graceful time.Duration) error {
	return m.inner.Stop(ctx, graceful)
}

func (m *meteredMachine) Status() types.VMStatus { return m.inner.Status() }

func (m *meteredMachine) DialAgent(ctx context.Context) (*guest.Client, error) {
	return m.inner.DialAgent(ctx)
}

func (m *meteredMachine) Logs() io.ReadCloser { return m.inner.Logs() }

type meteredConsole struct{ meteredMachine }

func (m *meteredConsole) Console() (io.ReadWriteCloser, error) {
	return m.inner.(runtime.Consoler).Console()
}
