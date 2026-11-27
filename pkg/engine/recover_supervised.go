package engine

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"
	"github.com/mlvea/darwin-node/pkg/volume"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// supervisorRecoverer is implemented by the supervised runtime. Fake and
// vz are not, so a process restart with --vm-supervisor left off keeps
// the node-restart settlement. Meter forwards the method; the engine
// does not import the adapter.
type supervisorRecoverer interface {
	Recover(context.Context, []supervisor.PodPresence) ([]runtime.Machine, error)
}

// recoveredStart is a supervisor VM claimed for one pod. ctx is the
// pod context Create's cancel stops.
type recoveredStart struct {
	rec *podRecord
	ctx context.Context
}

// recoverSupervisors adopts VMs whose supervisors survived this process.
// A match keeps the slot, so the cache sweep does not delete the disk.
// A supervisor with no pod directory is stopped inside Recover. Callers
// that do not implement the hook are left on the node-restart path.
func (e *Engine) recoverSupervisors() error {
	if e == nil || e.rt == nil {
		return nil
	}
	e.installSkewControl()
	rec, ok := e.rt.(supervisorRecoverer)
	if !ok || rec == nil {
		return nil
	}
	presence := e.supervisorPresence()
	known := make(map[string]struct{}, len(presence))
	for _, p := range presence {
		known[p.UID] = struct{}{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	machines, err := rec.Recover(ctx, presence)
	if err != nil {
		for _, m := range machines {
			e.stopSupervisorMachine(ctx, m, false)
		}
		return err
	}
	for _, m := range machines {
		if m == nil {
			continue
		}
		id := m.ID()
		if _, ok := known[id.UID]; !ok || id.UID == "" {
			slog.Warn("supervisor recovery: stopping VM with no pod directory", "uid", id.UID)
			e.stopSupervisorMachine(ctx, m, false)
			continue
		}
		if e.slots != nil {
			if aerr := e.slots.TryAcquire(id.UID); aerr != nil {
				slog.Warn("supervisor recovery: slot refused", "uid", id.UID, "err", aerr)
				e.stopSupervisorMachine(ctx, m, false)
				continue
			}
		}
		e.mu.Lock()
		if e.adopted == nil {
			e.adopted = map[string]runtime.Machine{}
		}
		if prev := e.adopted[id.UID]; prev != nil {
			e.mu.Unlock()
			slog.Warn("supervisor recovery: duplicate VM for pod", "uid", id.UID)
			e.stopSupervisorMachine(ctx, m, false)
			continue
		}
		e.adopted[id.UID] = m
		e.mu.Unlock()
		slog.Info("supervisor recovery: VM still running", "uid", id.UID, "pod", id.Namespace+"/"+id.Name)
	}
	return nil
}

func (e *Engine) supervisorPresence() []supervisor.PodPresence {
	if e == nil {
		return nil
	}
	uids := interruptedPodUIDs(e.cfg.CacheDir)
	out := make([]supervisor.PodPresence, 0, len(uids))
	for _, uid := range uids {
		// Namespace and name are not known until virtual-kubelet
		// resubmits the pod. The lease already carries them, and an
		// empty value skips that half of the fence.
		out = append(out, supervisor.PodPresence{UID: uid, Found: true})
	}
	return out
}

// claimRecoveredLocked takes a reattached VM for this pod. The caller
// holds e.mu. A nil start and a nil discard means there was nothing to
// claim. A discard is a VM that must be stopped; settlement then runs.
func (e *Engine) claimRecoveredLocked(pod *corev1.Pod) (*recoveredStart, runtime.Machine) {
	if e == nil || pod == nil || e.draining.Load() || e.adopted == nil {
		return nil, nil
	}
	uid := string(pod.UID)
	m := e.adopted[uid]
	if m == nil {
		return nil, nil
	}
	delete(e.adopted, uid)
	if !supervisorPodAttachable(pod, m.ID()) {
		return nil, m
	}
	pctx, cancel := context.WithCancel(context.Background())
	cp := pod.DeepCopy()
	rec := &podRecord{
		pod:          cp,
		machine:      m,
		phase:        corev1.PodPending,
		reason:       "Starting",
		cancel:       cancel,
		restartCount: priorRestartCount(cp),
		initialized:  true,
	}
	if cp.Status.StartTime != nil {
		st := *cp.Status.StartTime
		rec.startedAt = &st
	}
	rec.startDone.Add(1)
	delete(e.interrupted, uid)
	delete(e.restartCredit, uid)
	e.pods[Key(cp.Namespace, cp.Name)] = rec
	return &recoveredStart{rec: rec, ctx: pctx}, nil
}

// supervisorPodAttachable is a live VM whose identity matches the pod
// virtual-kubelet just submitted. A terminal or deleting pod is not
// brought back to Running.
func supervisorPodAttachable(pod *corev1.Pod, id types.MachineID) bool {
	if pod == nil || pod.DeletionTimestamp != nil || podTerminal(pod) {
		return false
	}
	if id.UID != "" && id.UID != string(pod.UID) {
		return false
	}
	if id.Namespace != "" && id.Namespace != pod.Namespace {
		return false
	}
	if id.Name != "" && id.Name != pod.Name {
		return false
	}
	return true
}

// launchRecovered reserves host ports and redials the guest. The VM is
// already running. This does not boot another one.
func (e *Engine) launchRecovered(start *recoveredStart, creds Credentials) error {
	if e == nil || start == nil || start.rec == nil {
		return nil
	}
	rec := start.rec
	ctx := start.ctx
	rec.mu.Lock()
	pod := rec.pod
	cancel := rec.cancel
	machine := rec.machine
	rec.mu.Unlock()
	if pod == nil {
		if cancel != nil {
			cancel()
		}
		rec.startDone.Done()
		return nil
	}
	key := Key(pod.Namespace, pod.Name)
	bindHost := e.hostPortBindHost()
	rec.mu.Lock()
	rec.hostPortBind = bindHost
	rec.hostPortBindSet = true
	rec.mu.Unlock()
	if maps := hostPortReservations(pod, bindHost); len(maps) > 0 {
		if err := e.hostports.Reserve(key, maps); err != nil {
			e.mu.Lock()
			delete(e.pods, key)
			e.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			e.stopSupervisorMachine(context.Background(), machine, true)
			rec.startDone.Done()
			return err
		}
	}
	e.events.Normal(ctx, event.ReasonStarting, "reattaching live macOS VM")
	go func() {
		defer rec.startDone.Done()
		e.runRecovered(ctx, rec, creds)
	}()
	return nil
}

// runRecovered redials the guest through the adopted fd and marks the
// pod Running. A handshake that already has an exit code is published
// and the VM is stopped; that workload is finished. A handshake that
// says the workload already started does not call Workload again.
// Otherwise the container command is applied once. Sidecars died with
// the agent, so they are created again. Init is not reapplied.
func (e *Engine) runRecovered(ctx context.Context, rec *podRecord, creds Credentials) {
	rec.mu.Lock()
	pod := rec.pod.DeepCopy()
	machine := rec.machine
	rec.mu.Unlock()
	if pod == nil || machine == nil {
		return
	}
	if code, ok := supervisorWorkloadResult(machine); ok {
		e.finishSupervisorWorkload(rec, code)
		return
	}
	token := e.recoveredAgentToken(string(pod.UID))
	if setter, ok := machine.(agentTokenSetter); ok {
		setter.SetAgentToken(token)
	}
	rec.mu.Lock()
	rec.token = token
	rec.mu.Unlock()
	e.serveConsole(rec)

	e.events.Normal(ctx, event.ReasonVMDialing, "redialing guest agent on adopted VM")
	cli, err := e.dialGuest(ctx, rec, machine)
	if err != nil {
		if ctx.Err() == nil {
			e.fail(rec, err)
		}
		return
	}
	reported := ""
	if netinfo, nerr := cli.NetInfo(ctx); nerr == nil {
		reported = netinfo.PrimaryIP
	}
	e.notePodIP(ctx, rec, machine, reported)

	rec.mu.Lock()
	rec.machine = machine
	rec.agent = cli
	rec.token = token
	rec.mu.Unlock()
	e.watchGuestExit(rec, cli)

	volRoot := filepath.Join(e.cfg.CacheDir, "pods", string(pod.UID), "volumes")
	limits, lerr := volume.EmptyDirLimits(pod, volRoot)
	if lerr != nil {
		e.fail(rec, lerr)
		return
	}
	// Host files only. The guest already has these mounts from the
	// previous process, so this does not call Materialize on the agent.
	_, places, merr := volume.Materialize(creds.volumeReq(pod, pod.Spec.Containers[0], volRoot, e.cfg.AllowedHostPaths))
	if merr != nil {
		e.fail(rec, merr)
		return
	}
	rec.mu.Lock()
	rec.emptyDirs = limits
	rec.volRoot = volRoot
	rec.places = places
	rec.tokens = append([]volume.ProjectedToken(nil), creds.Tokens...)
	rec.refreshAt = nextTokenRefresh(rec.tokens, time.Now())
	rec.mu.Unlock()

	for i := 1; i < len(pod.Spec.Containers); i++ {
		if _, _, serr := volume.Materialize(creds.volumeReq(pod, pod.Spec.Containers[i], volRoot, e.cfg.AllowedHostPaths)); serr != nil {
			e.fail(rec, serr)
			return
		}
		if cerr := e.sidecar.Create(sidecar.WithCreateOptions(ctx, e.sidecarCreateOptions(creds)), pod, pod.Spec.Containers[i], volRoot); cerr != nil {
			e.fail(rec, cerr)
			return
		}
	}
	e.refreshSidecarStatus(ctx, rec)
	if !e.noteSidecarExit(rec) {
		return
	}
	if err := e.applyRecoveredWorkload(ctx, cli, pod, machine); err != nil {
		e.fail(rec, err)
		return
	}

	now := metav1.Now()
	rec.mu.Lock()
	rec.machine = machine
	rec.agent = cli
	rec.token = token
	rec.phase = corev1.PodRunning
	rec.reason = ""
	rec.message = ""
	if rec.startedAt == nil {
		rec.startedAt = &now
	}
	rec.agentOK = true
	rec.startupDone = !hasStartupProbe(pod.Spec.Containers[0])
	_, hasReadyProbe := k8sProbe(pod.Spec.Containers[0].ReadinessProbe, pod.Spec.Containers[0].Ports)
	rec.ready = rec.startupDone && !hasReadyProbe
	ip := rec.pod.Status.PodIP
	rec.mu.Unlock()
	if err := e.syncHostPorts(rec, pod, ip); err != nil {
		e.events.Warn(ctx, event.ReasonFailed, "hostPort: "+err.Error())
		e.fail(rec, err)
		return
	}
	e.events.Normal(ctx, event.ReasonStarted, "reattached live macOS VM; guest agent ready")
	rec.watchDone.Add(1)
	go func() {
		defer rec.watchDone.Done()
		e.watch(ctx, rec)
	}()
}

func (e *Engine) recoveredAgentToken(uid string) string {
	if e == nil || uid == "" || e.cfg.CacheDir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(e.cfg.CacheDir, "pods", uid, "control", types.GuestAgentTokenFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// stopSupervisorMachine stops a VM this process adopted and, when release
// is set, gives the engine slot back. A Stop that does not return keeps
// the slot so a VM that might still be running is still counted.
func (e *Engine) stopSupervisorMachine(ctx context.Context, m runtime.Machine, release bool) {
	if e == nil || m == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	uid := m.ID().UID
	stopCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := m.Stop(stopCtx, 0)
	cancel()
	if err != nil {
		slog.Warn("supervisor recovery: stop failed", "uid", uid, "err", err)
		return
	}
	if release && e.slots != nil && uid != "" {
		e.slots.Release(uid)
	}
}

// stopAdoptedMachines stops supervisor VMs Create never claimed. A Stop
// that outlives the close deadline leaves the slot held.
func (e *Engine) stopAdoptedMachines() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	adopted := e.adopted
	e.adopted = nil
	e.mu.Unlock()
	if len(adopted) == 0 {
		return nil
	}
	d := e.closeDeadline()
	var errs []error
	for uid, m := range adopted {
		if m == nil {
			if e.slots != nil {
				e.slots.Release(uid)
			}
			continue
		}
		id := uid
		if id == "" {
			id = "supervisor"
		}
		boot := abandonedBoot{ID: id, Kind: abandonedPodKind, Ref: id}
		err := e.callWithin(d, &boot, func() {
			_ = m.Stop(context.Background(), 0)
		})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if e.slots != nil && uid != "" {
			e.slots.Release(uid)
		}
	}
	return errors.Join(errs...)
}
