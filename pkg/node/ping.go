package node

import (
	"context"
	"fmt"
	"os"
	"strings"
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
	// Runtime reports whether a VM can be booted. Nil skips the check.
	Runtime func(context.Context) error
	// CacheDir is checked for writability when non-empty.
	CacheDir string
	// conds is the last node status we published. NotifyNodeStatus builds
	// a blank Node every period; without this, every publish looks like a
	// transition.
	conds []corev1.NodeCondition
	// skew cordons the node while a supervisor wire major does not match.
	// Nil leaves spec.unschedulable alone.
	skew *SkewCordon
}

// NotReady is nil when the last probe succeeded. Pod creates use it to
// reject work while the node conditions say NotReady.
func (p *StatusProvider) NotReady() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return notReady(p.inv)
}

func notReady(inv Inventory) error {
	msg := unreadyMessage(inv)
	if msg == "" {
		return nil
	}
	return fmt.Errorf("node not ready: %s", msg)
}

func unreadyMessage(inv Inventory) string {
	var parts []string
	if inv.ProbeFailed != "" {
		parts = append(parts, inv.ProbeFailed)
	}
	if inv.RuntimeFailed != "" {
		parts = append(parts, inv.RuntimeFailed)
	}
	if inv.CacheFailed != "" {
		parts = append(parts, inv.CacheFailed)
	}
	return strings.Join(parts, "; ")
}

func NewStatusProvider(inv Inventory) *StatusProvider {
	return &StatusProvider{inv: inv, Probe: ProbeHost}
}

func (p *StatusProvider) Ping(ctx context.Context) error {
	p.mu.Lock()
	probe := p.Probe
	runtimeCheck := p.Runtime
	cacheDir := p.CacheDir
	p.mu.Unlock()
	if probe == nil {
		probe = ProbeHost
	}
	h, herr := probe(ctx)
	var rerr error
	if runtimeCheck != nil {
		rerr = runtimeCheck(ctx)
	}
	var cerr error
	if cacheDir != "" {
		cerr = checkCacheWritable(cacheDir)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	// Record the failure and return nil. Virtual-kubelet skips the node
	// status patch and the lease renew when Ping returns an error, which
	// would freeze the last Ready=True condition. The NotReady bit has to
	// travel in the status update itself.
	if herr != nil {
		p.inv.ProbeFailed = herr.Error()
	} else {
		p.inv.Host = h
		p.inv.ProbeFailed = ""
	}
	if runtimeCheck != nil {
		if rerr != nil {
			p.inv.RuntimeFailed = rerr.Error()
		} else {
			p.inv.RuntimeFailed = ""
		}
	}
	if cacheDir != "" {
		if cerr != nil {
			p.inv.CacheFailed = cerr.Error()
		} else {
			p.inv.CacheFailed = ""
		}
	}
	return nil
}

func checkCacheWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("cache dir not writable: %w", err)
	}
	f, err := os.CreateTemp(dir, ".probe-*")
	if err != nil {
		return fmt.Errorf("cache dir not writable: %w", err)
	}
	name := f.Name()
	cerr := f.Close()
	rerr := os.Remove(name)
	if cerr != nil {
		return fmt.Errorf("cache dir not writable: %w", cerr)
	}
	if rerr != nil {
		return fmt.Errorf("cache dir not writable: %w", rerr)
	}
	return nil
}

func (p *StatusProvider) NotifyNodeStatus(ctx context.Context, cb func(*corev1.Node)) {
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		send := func() {
			n := p.nodeSnapshot()
			if n == nil {
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

func (p *StatusProvider) SetSkewCordon(s *SkewCordon) {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.skew = s
	p.mu.Unlock()
}

// nodeSnapshot builds the node we would publish. The previous conditions
// are seeded first so LastTransitionTime survives a blank Node. The skew
// cordon is applied after Apply, which rebuilds conditions from the
// inventory and would otherwise drop it.
func (p *StatusProvider) nodeSnapshot() *corev1.Node {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	inv := p.inv
	prev := append([]corev1.NodeCondition(nil), p.conds...)
	skew := p.skew
	p.mu.Unlock()
	n := &corev1.Node{}
	if len(prev) > 0 {
		n.Status.Conditions = prev
	}
	if err := Apply(context.Background(), n, inv); err != nil {
		return nil
	}
	if skew != nil {
		skew.Apply(n)
	}
	p.mu.Lock()
	p.conds = append([]corev1.NodeCondition(nil), n.Status.Conditions...)
	p.mu.Unlock()
	return n
}

func (p *StatusProvider) Inventory() Inventory {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.inv
}
