package supervised

import (
	"context"
	"fmt"

	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"
)

// Recover dials supervisors that are already listening. It does not spawn.
// A slot whose boot id and lease still match a live pod is returned still
// running. Anything else is stopped and the lease is removed.
func (r *Runtime) Recover(ctx context.Context, pods []supervisor.PodPresence) ([]runtime.Machine, error) {
	n := r.slotCount()
	var out []runtime.Machine
	for slot := 0; slot < n; slot++ {
		c, err := r.dialSlot(ctx, slot)
		if err != nil {
			continue
		}
		m, err := r.adopt(ctx, slot, c, pods)
		if err != nil {
			_ = c.StopVM(ctx, 0)
			_ = c.Close()
			_ = supervisor.RemoveLease(r.RunDir, slot)
			r.free(slot)
			continue
		}
		if m == nil {
			_ = c.Close()
			_ = supervisor.RemoveLease(r.RunDir, slot)
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (r *Runtime) adopt(ctx context.Context, slot int, c *supervisor.Client, pods []supervisor.PodPresence) (*machine, error) {
	insp, err := c.InspectVM(ctx)
	if err != nil {
		return nil, err
	}
	if insp.BootID == "" {
		return nil, nil
	}
	lease, err := supervisor.ReadLease(r.RunDir, slot)
	if err != nil {
		return nil, err
	}
	pod := presenceFor(pods, lease)
	if err := supervisor.Fence(lease, insp, pod); err != nil {
		return nil, fmt.Errorf("%w", err)
	}
	r.mark(slot)
	m := &machine{
		rt:     r,
		id:     types.MachineID{Namespace: insp.Namespace, Name: insp.PodName, UID: insp.PodUID},
		spec:   types.VMSpec{ID: types.MachineID{Namespace: insp.Namespace, Name: insp.PodName, UID: insp.PodUID}, ImageRef: insp.ImageDigest, AgentToken: ""},
		slot:   slot,
		client: c,
		status: statusFrom(insp),
	}
	m.startHeartbeat()
	return m, nil
}

func presenceFor(pods []supervisor.PodPresence, lease supervisor.Lease) supervisor.PodPresence {
	for _, p := range pods {
		if p.UID != lease.PodUID {
			continue
		}
		if p.Namespace != "" && p.Namespace != lease.Namespace {
			continue
		}
		if p.Name != "" && p.Name != lease.PodName {
			continue
		}
		return p
	}
	return supervisor.PodPresence{Found: false, UID: lease.PodUID}
}
