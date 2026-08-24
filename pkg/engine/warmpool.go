// Warm VM pool: keep pre-booted idle macOS VMs in otherwise-free slots so
// matching pods adopt a running guest instead of cold-booting one.
//
// Invariants:
//   - A warm VM holds a real slot from the same fail-closed table as pods;
//     the pool only ever fills capacity pods do not need right now.
//   - Real demand evicts warm entries before any pod is rejected.
//   - Only pods whose primary container mounts nothing can adopt: virtio-fs
//     shares are fixed at VM creation and cannot be attached post hoc.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/darwin-node/darwin-node/internal/netutil"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

type warmEntry struct {
	seq     int
	slotID  string // uid holding the slot
	imgRef  string
	root    string // cache/warm/<seq> (overlay, control)
	token   string
	mac     string
	cpu     uint
	mem     uint64
	machine runtime.Machine
}

// warmTickInterval balances adoption latency against per-tick work.
const warmTickInterval = time.Second

type warmPool struct {
	mu      sync.Mutex
	entries []*warmEntry
	seq     int
}

func (p *warmPool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// takeLIFO removes and returns the newest entry matching ref, or nil.
func (p *warmPool) take(ref string) *warmEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.entries) - 1; i >= 0; i-- {
		if p.entries[i].imgRef == ref {
			e := p.entries[i]
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			return e
		}
	}
	return nil
}

// popOldest removes and returns the longest-idle entry, or nil.
func (p *warmPool) popOldest() *warmEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) == 0 {
		return nil
	}
	e := p.entries[0]
	p.entries = p.entries[1:]
	return e
}

// snapshot returns a copy of the current entries.
func (p *warmPool) snapshot() []*warmEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]*warmEntry, len(p.entries))
	copy(out, p.entries)
	return out
}

// drain removes and returns every entry.
func (p *warmPool) drain() []*warmEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.entries
	p.entries = nil
	return out
}

func (p *warmPool) add(e *warmEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append(p.entries, e)
}

// startWarmPool launches the replenisher. Called once from New.
func (e *Engine) startWarmPool() {
	ctx, cancel := context.WithCancel(context.Background())
	e.warmCtx = ctx
	e.warmCancel = cancel
	go func() {
		ticker := time.NewTicker(warmTickInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				e.fillWarm(ctx)
			}
		}
	}()
}

func (e *Engine) warmImageRef() string {
	if e.cfg.WarmImage != "" {
		return e.cfg.WarmImage
	}
	if v, ok := e.lastImage.Load().(string); ok {
		return v
	}
	return ""
}

// noteImageRef records the most recently booted pod image so the pool can
// warm it when no explicit --warm-image is configured.
func (e *Engine) noteImageRef(ref string) {
	if ref != "" {
		e.lastImage.Store(ref)
	}
}

// fillWarm tops the pool up while free slots and the configured budget allow.
func (e *Engine) fillWarm(ctx context.Context) {
	ref := e.warmImageRef()
	if ref == "" {
		return
	}
	for ctx.Err() == nil {
		budget := min(e.cfg.WarmSlots-e.warm.len(), e.slots.Free())
		if budget <= 0 {
			return
		}
		if err := e.bootWarm(ctx, ref); err != nil {
			if !errors.Is(err, capacity.ErrVMCapacityExhausted) {
				slog.Warn("warm pool", "boot", err)
			}
			return
		}
	}
}

// bootWarm pre-boots one idle VM from ref into a freshly acquired slot.
func (e *Engine) bootWarm(ctx context.Context, ref string) error {
	if e.warmCtx != nil && e.warmCtx.Err() != nil {
		return context.Canceled
	}
	e.warmBootWG.Add(1)
	defer e.warmBootWG.Done()

	e.warm.mu.Lock()
	e.warm.seq++
	seq := e.warm.seq
	e.warm.mu.Unlock()

	slotID := fmt.Sprintf("darwin-warm-%d", seq)
	if err := e.slots.TryAcquire(slotID); err != nil {
		return err
	}
	root := filepath.Join(e.cfg.CacheDir, "warm", fmt.Sprint(seq))
	token, mac, cpu, mem, machine, err := e.bootWarmMachine(ctx, slotID, ref, root)
	if err != nil {
		e.slots.Release(slotID)
		_ = os.RemoveAll(root)
		return err
	}
	e.warm.add(&warmEntry{
		seq: seq, slotID: slotID, imgRef: ref, root: root,
		token: token, mac: mac, cpu: cpu, mem: mem, machine: machine,
	})
	e.events.Normal(context.Background(), event.ReasonWarmBooted,
		fmt.Sprintf("slot %s pre-booted %q; pods with this image start instantly", slotID, ref))
	return nil
}

// bootWarmMachine resolves the image, creates and starts the machine, and
// waits for agent readiness so adoption later is genuinely instant.
func (e *Engine) bootWarmMachine(ctx context.Context, slotID, ref, root string) (token, mac string, cpu uint, mem uint64, m runtime.Machine, err error) {
	token, err = randomToken()
	if err != nil {
		return "", "", 0, 0, nil, err
	}
	hw, err := netutil.RandomMAC()
	if err != nil {
		return "", "", 0, 0, nil, err
	}
	mac = hw.String()

	ctrl := filepath.Join(root, "control")
	if err := os.MkdirAll(ctrl, 0o700); err != nil {
		return "", "", 0, 0, nil, err
	}
	if err := os.WriteFile(filepath.Join(ctrl, types.GuestAgentTokenFile), []byte(token), 0o600); err != nil {
		return "", "", 0, 0, nil, err
	}
	shares := []types.Share{{Name: types.ControlShareName, HostPath: ctrl, ReadOnly: true}}

	cpu, mem = 2, uint64(4<<30)
	spec := types.VMSpec{
		ID:           types.MachineID{Name: slotID, UID: slotID},
		ImageRef:     ref,
		CPU:          cpu,
		MemoryBytes:  mem,
		MAC:          mac,
		NetworkMode:  e.cfg.NetworkMode,
		BridgeDevice: e.cfg.BridgeInterface,
		Graphics:     e.cfg.Graphics,
		Shares:       shares,
		AgentToken:   token,
	}
	if e.rt.Name() == types.RuntimeVZ {
		disk, aux, hwm, rerr := e.resolveImage(ctx, ref, slotID, corev1.PullIfNotPresent, Credentials{})
		if rerr != nil {
			return "", "", 0, 0, nil, rerr
		}
		spec.DiskPath = disk
		spec.AuxPath = aux
		spec.HardwareModelData = hwm
	}
	mach, err := e.rt.Create(ctx, spec)
	if err != nil {
		return "", "", 0, 0, nil, err
	}
	if err := mach.Start(ctx); err != nil {
		return "", "", 0, 0, nil, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, e.cfg.AgentReadyTimeout)
	defer cancel()
	cli, err := mach.DialAgent(readyCtx)
	if err == nil {
		var ready guest.ReadyRes
		ready, err = cli.Ready(readyCtx)
		if err == nil && !ready.Ready {
			err = errors.New("agent not ready")
		}
	}
	if err != nil {
		_ = mach.Stop(context.Background(), 5*time.Second)
		return "", "", 0, 0, nil, fmt.Errorf("warm boot %s: %w", slotID, err)
	}
	return token, mac, cpu, mem, mach, nil
}

// takeWarm adopts a pre-booted VM for pod's image, or returns nil.
func (e *Engine) takeWarm(imageRef string) *warmEntry {
	entry := e.warm.take(imageRef)
	if entry == nil {
		return nil
	}
	e.events.Normal(context.Background(), event.ReasonWarmAdopted,
		fmt.Sprintf("pod adopted pre-booted VM %s (no cold boot)", entry.slotID))
	return entry
}

// canAdopt reports whether a pod can run on a pre-booted VM: virtio-fs
// shares are fixed at creation, so container[0] must mount nothing and
// declare no cache volumes.
func canAdopt(pod *corev1.Pod) bool {
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return false
	}
	if len(pod.Spec.Containers[0].VolumeMounts) != 0 {
		return false
	}
	caches, err := ParseCacheAnnotations(pod)
	return err == nil && len(caches) == 0
}

// reclaimWarmForPod frees capacity for real demand. It prefers a warm entry
// matching ref and returns it alive for direct adoption; otherwise it stops
// the oldest entry to make room. ok reports whether a slot was freed.
func (e *Engine) reclaimWarmForPod(ref string, adoptable bool) (entry *warmEntry, ok bool) {
	if adoptable {
		if entry = e.warm.take(ref); entry != nil {
			e.events.Normal(context.Background(), event.ReasonWarmAdopted,
				fmt.Sprintf("pending pod will adopt pre-booted VM %s", entry.slotID))
			return entry, true
		}
	}
	if entry = e.warm.popOldest(); entry != nil {
		slog.Info("warm pool: evicting for pod demand", "slot", entry.slotID)
		_ = entry.machine.Stop(context.Background(), 5*time.Second)
		e.slots.Release(entry.slotID)
		_ = os.RemoveAll(entry.root)
		e.events.Normal(context.Background(), event.ReasonWarmEvicted,
			"reclaimed slot "+entry.slotID+" for a pending pod")
		return nil, true
	}
	return nil, false
}

// shutdownWarmPool stops all warm VMs and releases their slots. It waits for
// in-flight boots to land first so nothing outlives the call.
func (e *Engine) shutdownWarmPool() {
	if e.warmCancel != nil {
		e.warmCancel()
	}
	e.warmBootWG.Wait()
	for _, entry := range e.warm.drain() {
		_ = entry.machine.Stop(context.Background(), 5*time.Second)
		e.slots.Release(entry.slotID)
		_ = os.RemoveAll(entry.root)
	}
}
