package node

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// StatusProvider implements virtual-kubelet NodeProvider. Ping re-probes the
// host so Memory/Disk pressure can change after ConfigureNode.
type StatusProvider struct {
	mu    sync.Mutex
	inv   Inventory
	Probe func(context.Context) (Host, error)
}

func NewStatusProvider(inv Inventory) *StatusProvider {
	return &StatusProvider{inv: inv, Probe: ProbeHost}
}

func (p *StatusProvider) Ping(ctx context.Context) error {
	probe := p.Probe
	if probe == nil {
		probe = ProbeHost
	}
	h, err := probe(ctx)
	if err != nil {
		return nil
	}
	p.mu.Lock()
	p.inv.Host = h
	p.mu.Unlock()
	return nil
}

func (p *StatusProvider) NotifyNodeStatus(ctx context.Context, cb func(*corev1.Node)) {
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		send := func() {
			p.mu.Lock()
			inv := p.inv
			p.mu.Unlock()
			n := &corev1.Node{}
			if err := Apply(context.Background(), n, inv); err != nil {
				return
			}
			cb(n)
		}
		send()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = p.Ping(ctx)
				send()
			}
		}
	}()
}

func (p *StatusProvider) Inventory() Inventory {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inv
}
