// Warm VM pool: keep pre-booted idle macOS VMs in otherwise-free slots so
// matching pods adopt a running guest instead of cold-booting one.
//
// Invariants:
//   - A warm VM holds a real slot from the same fail-closed table as pods;
//     the pool only ever fills capacity pods do not need right now.
//   - A real pod preempts an in-flight warm boot and evicts a pooled VM
//     that it cannot adopt.
//   - Only a warm VM whose image and size match, and whose image the pod
//     is allowed to pull, is adopted. The adopted guest gets a new token.
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

	"github.com/mlvea/darwin-node/internal/netutil"
	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

type warmEntry struct {
	seq     int
	slotID  string // uid holding the slot
	imgRef  string
	root    string // cache/warm/<seq> (control files)
	diskDir string // cache/pods/<slotID> CoW overlay (multi-GB)
	token   string
	mac     string
	cpu     uint
	mem     uint64
	machine runtime.Machine
	// probe is the readiness dial. The guest agent serves that one
	// connection until it is closed, then dials again. It stays open while
	// the VM is idle so the guest is not left waiting on a handshake nobody
	// will accept, and it is closed when a pod is about to dial or the VM
	// is stopped.
	probe *guest.Client
}

// warmTickInterval balances adoption latency against per-tick work.
const warmTickInterval = time.Second

const (
	warmBootBackoffInitial = time.Second
	warmBootBackoffMax     = 30 * time.Second
)

// warmFlight is one bootWarm that has not finished. Real pods cancel it
// so a warm boot cannot pin the only slot.
type warmFlight struct {
	cancel context.CancelFunc
	done   chan struct{}
}

type warmPool struct {
	mu        sync.Mutex
	entries   []*warmEntry
	seq       int
	closed    bool
	flights   []*warmFlight
	nextBoot  time.Time
	failDelay time.Duration
}

func (p *warmPool) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// hasSized reports whether a pooled VM matches the image and the size.
func (p *warmPool) hasSized(ref string, cpu uint, mem uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, e := range p.entries {
		if e.imgRef == ref && e.cpu == cpu && e.mem == mem {
			return true
		}
	}
	return false
}

// takeSized removes the newest entry matching ref and size, or nil.
func (p *warmPool) takeSized(ref string, cpu uint, mem uint64) *warmEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i := len(p.entries) - 1; i >= 0; i-- {
		e := p.entries[i]
		if e.imgRef == ref && e.cpu == cpu && e.mem == mem {
			p.entries = append(p.entries[:i], p.entries[i+1:]...)
			return e
		}
	}
	return nil
}

// drop removes the entry with slotID. nil means it was already taken.
func (p *warmPool) drop(slotID string) *warmEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, e := range p.entries {
		if e.slotID == slotID {
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

// addIfOpen appends e unless shutdown has started. false means the caller
// still owns e and must stop it.
func (p *warmPool) addIfOpen(e *warmEntry) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false
	}
	p.entries = append(p.entries, e)
	return true
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

// fillWarm drops crashed warm VMs, then tops the pool up while free slots
// and the configured budget allow. A failed boot waits, doubling from 1s
// to 30s. Capacity and cancellation do not count as failures.
func (e *Engine) fillWarm(ctx context.Context) {
	if e.draining.Load() {
		return // freed slots during a drain belong to departing pods, not the pool
	}
	e.sweepCrashedWarm()
	if !e.bootAllowed() {
		return
	}
	ref := e.warmImageRef()
	if ref == "" {
		return
	}
	for ctx.Err() == nil {
		// WarmSlots is fixed at New. Production never writes it again;
		// the replenisher reads the field with no lock. Tests that drive
		// fillWarm themselves set it before the call.
		budget := min(e.cfg.WarmSlots-e.warm.len(), e.slots.Free())
		if budget <= 0 {
			return
		}
		if err := e.bootWarm(ctx, ref); err != nil {
			if !errors.Is(err, capacity.ErrVMCapacityExhausted) && !errors.Is(err, context.Canceled) {
				e.noteBootFailure()
				slog.Warn("warm pool", "boot", err)
			}
			return
		}
		e.noteBootSuccess()
	}
}

func (e *Engine) bootAllowed() bool {
	e.warm.mu.Lock()
	defer e.warm.mu.Unlock()
	if e.warm.nextBoot.IsZero() {
		return true
	}
	return !e.now().Before(e.warm.nextBoot)
}

func (e *Engine) noteBootFailure() {
	e.warm.mu.Lock()
	defer e.warm.mu.Unlock()
	d := e.warm.failDelay
	if d < warmBootBackoffInitial {
		d = warmBootBackoffInitial
	}
	e.warm.nextBoot = e.now().Add(d)
	next := d * 2
	if next > warmBootBackoffMax {
		next = warmBootBackoffMax
	}
	e.warm.failDelay = next
}

func (e *Engine) noteBootSuccess() {
	e.warm.mu.Lock()
	defer e.warm.mu.Unlock()
	e.warm.nextBoot = time.Time{}
	e.warm.failDelay = 0
}

func (e *Engine) warmBooting() int {
	e.warm.mu.Lock()
	defer e.warm.mu.Unlock()
	return len(e.warm.flights)
}

func (e *Engine) addFlight(f *warmFlight) {
	e.warm.mu.Lock()
	e.warm.flights = append(e.warm.flights, f)
	e.warm.mu.Unlock()
}

func (e *Engine) removeFlight(f *warmFlight) {
	e.warm.mu.Lock()
	defer e.warm.mu.Unlock()
	for i, g := range e.warm.flights {
		if g == f {
			e.warm.flights = append(e.warm.flights[:i], e.warm.flights[i+1:]...)
			return
		}
	}
}

// preemptOneWarmBoot cancels the oldest in-flight warm boot and waits until
// its slot has been released.
func (e *Engine) preemptOneWarmBoot() bool {
	e.warm.mu.Lock()
	if len(e.warm.flights) == 0 {
		e.warm.mu.Unlock()
		return false
	}
	f := e.warm.flights[0]
	e.warm.flights = e.warm.flights[1:]
	e.warm.mu.Unlock()
	f.cancel()
	<-f.done
	return true
}

// sweepCrashedWarm removes pooled VMs that have stopped or failed. A warm
// boot still in Starting is left alone; a real pod preempts that separately.
func (e *Engine) sweepCrashedWarm() {
	for _, entry := range e.warm.snapshot() {
		if entry == nil || entry.machine == nil {
			continue
		}
		st := entry.machine.Status()
		if st.State != types.VMStopped && st.State != types.VMFailed {
			continue
		}
		if e.warm.drop(entry.slotID) == nil {
			continue
		}
		slog.Info("warm pool: removing crashed VM", "slot", entry.slotID, "state", st.State)
		e.stopWarmEntry(entry)
	}
}

// bootWarm pre-boots one idle VM from ref into a freshly acquired slot.
// beginWarmBoot reserves a WaitGroup slot unless shutdown has already closed
// the pool. Add and the closed flag share warm.mu so Wait cannot return
// before an in-flight boot is tracked.
func (e *Engine) beginWarmBoot() bool {
	e.warm.mu.Lock()
	defer e.warm.mu.Unlock()
	if e.warm.closed {
		return false
	}
	e.warmBootWG.Add(1)
	return true
}

func (e *Engine) bootWarm(ctx context.Context, ref string) error {
	if !e.beginWarmBoot() {
		return context.Canceled
	}
	defer e.warmBootWG.Done()

	// Registered before Create so a real pod can cancel a boot that is
	// already inside the runtime.
	bootCtx, cancel := context.WithCancel(ctx)
	flight := &warmFlight{cancel: cancel, done: make(chan struct{})}
	e.addFlight(flight)
	defer func() {
		cancel()
		e.removeFlight(flight)
		close(flight.done)
	}()

	e.warm.mu.Lock()
	if e.warm.closed {
		e.warm.mu.Unlock()
		return context.Canceled
	}
	e.warm.seq++
	seq := e.warm.seq
	e.warm.mu.Unlock()

	slotID := fmt.Sprintf("darwin-warm-%d", seq)
	if err := e.slots.TryAcquire(slotID); err != nil {
		return err
	}
	e.trackBoot(abandonedBoot{ID: slotID, Kind: abandonedWarmKind, Ref: slotID})
	defer e.untrackBoot(slotID)
	root := filepath.Join(e.cfg.CacheDir, "warm", fmt.Sprint(seq))
	diskDir := podMACDir(e.cfg.CacheDir, slotID)
	token, mac, cpu, mem, machine, probe, err := e.bootWarmMachine(bootCtx, slotID, ref, root)
	entry := &warmEntry{
		seq: seq, slotID: slotID, imgRef: ref, root: root, diskDir: diskDir,
		token: token, mac: mac, cpu: cpu, mem: mem, machine: machine, probe: probe,
	}
	if err != nil {
		// bootWarmMachine stops a VM that Create/Start left running.
		// Clear it so stopWarmEntry does not stop twice, then release
		// the slot before flight.done is closed. A preempting pod waits
		// on that close and then acquires.
		if machine != nil {
			_ = machine.Stop(context.Background(), 0)
		}
		entry.machine = nil
		e.stopWarmEntry(entry)
		if errors.Is(bootCtx.Err(), context.Canceled) && ctx.Err() == nil {
			return context.Canceled
		}
		return err
	}
	if !e.warm.addIfOpen(entry) {
		e.stopWarmEntry(entry)
		return context.Canceled
	}
	e.events.Normal(context.Background(), event.ReasonWarmBooted,
		fmt.Sprintf("slot %s pre-booted %q; pods with this image start instantly", slotID, ref))
	return nil
}

// releaseWarmProbe closes the readiness dial. Safe to call twice.
func releaseWarmProbe(entry *warmEntry) {
	if entry == nil || entry.probe == nil {
		return
	}
	_ = entry.probe.Close()
	entry.probe = nil
}

// stopWarmEntry stops the VM, releases its slot, and deletes both the warm
// control dir and the CoW overlay. Release is idempotent.
func (e *Engine) stopWarmEntry(entry *warmEntry) {
	if entry == nil {
		return
	}
	releaseWarmProbe(entry)
	if entry.machine != nil {
		_ = entry.machine.Stop(context.Background(), 5*time.Second)
		entry.machine = nil
	}
	if entry.slotID != "" {
		e.slots.Release(entry.slotID)
	}
	if entry.root != "" {
		_ = os.RemoveAll(entry.root)
	}
	disk := entry.diskDir
	if disk == "" && entry.slotID != "" && e.cfg.CacheDir != "" {
		disk = podMACDir(e.cfg.CacheDir, entry.slotID)
	}
	if disk != "" {
		_ = os.RemoveAll(disk)
	}
}

// bootWarmMachine resolves the image, creates and starts the machine, and
// waits for agent readiness so adoption later is genuinely instant.
func (e *Engine) bootWarmMachine(ctx context.Context, slotID, ref, root string) (token, mac string, cpu uint, mem uint64, m runtime.Machine, probe *guest.Client, err error) {
	token, err = randomToken()
	if err != nil {
		return "", "", 0, 0, nil, nil, err
	}
	hw, err := netutil.RandomMAC()
	if err != nil {
		return "", "", 0, 0, nil, nil, err
	}
	mac = hw.String()

	ctrl := filepath.Join(root, "control")
	if err := os.MkdirAll(ctrl, 0o700); err != nil {
		return "", "", 0, 0, nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(ctrl, types.GuestAgentTokenFile), []byte(token), 0o600); err != nil {
		return "", "", 0, 0, nil, nil, err
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
			return "", "", 0, 0, nil, nil, rerr
		}
		spec.DiskPath = disk
		spec.AuxPath = aux
		spec.HardwareModelData = hwm
	}
	mach, err := e.rt.Create(ctx, spec)
	if err != nil {
		return "", "", 0, 0, nil, nil, err
	}
	if err := mach.Start(ctx); err != nil {
		// The vz runtime listens on vsock before vm.Start, and Start can
		// fail after the machine is already running. Returning a nil
		// machine released the slot and left that VM outside the cap.
		_ = mach.Stop(context.Background(), 0)
		return "", "", 0, 0, nil, nil, err
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
		// Caller closes cli. Returning it here covers the Ready failure
		// path; a dial error leaves cli nil.
		return "", "", 0, 0, nil, cli, fmt.Errorf("warm boot %s: %w", slotID, err)
	}
	return token, mac, cpu, mem, mach, cli, nil
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

// shutdownWarmPool stops all warm VMs and releases their slots. It waits for
// in-flight boots to land first so nothing outlives the call. A boot that
// does not return keeps its slot; Close logs that and does not release it.
func (e *Engine) shutdownWarmPool() error {
	if e.warmCancel != nil {
		e.warmCancel()
	}
	e.warm.mu.Lock()
	e.warm.closed = true
	e.warm.mu.Unlock()
	d := e.closeDeadline()
	var errs []error
	if err := e.callWithin(d, nil, e.warmBootWG.Wait); err != nil {
		msg := fmt.Sprintf("close: warm boot still in flight after %s; VM slot kept", d)
		slog.Error(msg)
		e.events.Warn(context.Background(), event.ReasonFailed, msg)
		errs = append(errs, errors.New(msg))
	}
	errs = append(errs, e.stopWarmEntries(d))
	return errors.Join(errs...)
}

// stopWarmEntries stops VMs that have already joined the pool. Each stop has
// its own deadline. A stop that does not return leaves that slot held.
func (e *Engine) stopWarmEntries(d time.Duration) error {
	entries := e.warm.drain()
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for _, entry := range entries {
		wg.Add(1)
		go func(entry *warmEntry) {
			defer wg.Done()
			boot := abandonedBoot{ID: entry.slotID, Kind: abandonedWarmKind, Ref: entry.slotID}
			if err := e.callWithin(d, &boot, func() { e.stopWarmEntry(entry) }); err == nil {
				return
			}
			msg := fmt.Sprintf("close: warm VM %s still stopping after %s; VM slot kept", entry.slotID, d)
			slog.Error(msg)
			e.events.Warn(context.Background(), event.ReasonFailed, msg)
			mu.Lock()
			errs = append(errs, errors.New(msg))
			mu.Unlock()
		}(entry)
	}
	wg.Wait()
	return errors.Join(errs...)
}
