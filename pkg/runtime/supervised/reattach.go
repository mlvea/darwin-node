package supervised

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"
)

// Recover adopts supervisors that are already listening. It does not
// spawn and it does not boot a VM. A slot whose lease still matches a
// live pod is returned running, on a guest fd from this call's
// AttachChannels. Anything else is stopped and the lease is removed.
func (r *Runtime) Recover(ctx context.Context, pods []supervisor.PodPresence) ([]runtime.Machine, error) {
	n := r.slotCount()
	var out []runtime.Machine
	for slot := 0; slot < n; slot++ {
		m, err := r.adoptExisting(ctx, slot, pods)
		if err != nil {
			r.stopSlot(ctx, slot)
			continue
		}
		if m == nil {
			continue
		}
		out = append(out, m)
	}
	return out, nil
}

func (r *Runtime) adoptExisting(ctx context.Context, slot int, pods []supervisor.PodPresence) (*machine, error) {
	lease, err := supervisor.ReadLease(r.RunDir, slot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			r.stopIfVMWithoutLease(ctx, slot)
			return nil, nil
		}
		return nil, err
	}
	m := &machine{
		rt:   r,
		slot: slot,
		id: types.MachineID{
			Namespace: lease.Namespace,
			Name:      lease.PodName,
			UID:       lease.PodUID,
		},
		spec: types.VMSpec{
			ID: types.MachineID{
				Namespace: lease.Namespace,
				Name:      lease.PodName,
				UID:       lease.PodUID,
			},
			ImageRef: lease.ImageDigest,
		},
		supPID:    lease.SupervisorPID,
		procStart: lease.SupervisorProcStart,
		hostBoot:  lease.HostBootID,
	}
	if err := m.adoptLive(ctx, lease, presenceFor(pods, lease)); err != nil {
		return nil, err
	}
	ctrl, err := r.dialSlot(ctx, slot)
	if err != nil {
		m.mu.Lock()
		sess := m.sess
		m.sess = nil
		m.mu.Unlock()
		if sess != nil {
			_ = sess.Close()
		}
		return nil, err
	}
	m.mu.Lock()
	m.client = ctrl
	lost := m.lostNoted
	if m.sess != nil && !lost {
		m.status = m.sess.Status()
	}
	m.mu.Unlock()
	if lost {
		_ = ctrl.Close()
		return nil, fmt.Errorf("supervisor lost")
	}
	r.mark(slot)
	m.startHeartbeat()
	return m, nil
}

// stopIfVMWithoutLease stops a supervisor that has a VM and no lease.
// A missing socket is left alone.
func (r *Runtime) stopIfVMWithoutLease(ctx context.Context, slot int) {
	c, err := r.dialSlot(ctx, slot)
	if err != nil {
		return
	}
	insp, err := c.InspectVM(ctx)
	if err != nil || insp.BootID == "" {
		_ = c.Close()
		return
	}
	_ = c.StopVM(ctx, 0)
	_ = c.Close()
}

func (r *Runtime) stopSlot(ctx context.Context, slot int) {
	c, err := r.dialSlot(ctx, slot)
	if err != nil {
		return
	}
	_ = c.StopVM(ctx, 0)
	_ = c.Close()
	_ = supervisor.RemoveLease(r.RunDir, slot)
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
