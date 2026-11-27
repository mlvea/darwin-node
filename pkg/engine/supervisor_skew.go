package engine

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/node"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/supervisor"

	corev1 "k8s.io/api/core/v1"
)

const skewPodReason = "SupervisorVersionSkew"

// skewBridges keeps the cordon for each engine without a field on Engine.
// The map holds the engine pointer for the process lifetime. One engine
// runs in production.
var skewBridges sync.Map

type holdState struct {
	reason string
	slot   bool
}

type skewBridge struct {
	e      *Engine
	cordon *node.SkewCordon
	mu     sync.Mutex
	held   map[string]holdState
}

func (e *Engine) installSkewControl() {
	if e == nil || e.rt == nil {
		return
	}
	setter, ok := e.rt.(interface{ SetSkewControl(supervisor.SkewControl) })
	if !ok {
		return
	}
	setter.SetSkewControl(e.skewBridge())
}

func (e *Engine) skewBridge() *skewBridge {
	if v, ok := skewBridges.Load(e); ok {
		return v.(*skewBridge)
	}
	b := &skewBridge{e: e, cordon: &node.SkewCordon{}, held: map[string]holdState{}}
	actual, _ := skewBridges.LoadOrStore(e, b)
	return actual.(*skewBridge)
}

// SupervisorSkew is the cordon the status publisher should apply. Nil
// when this engine's runtime does not adopt supervisors.
func (e *Engine) SupervisorSkew() *node.SkewCordon {
	if e == nil {
		return nil
	}
	v, ok := skewBridges.Load(e)
	if !ok {
		return nil
	}
	return v.(*skewBridge).cordon
}

func (e *Engine) skewHolding(uid string) bool {
	if e == nil || uid == "" {
		return false
	}
	v, ok := skewBridges.Load(e)
	if !ok {
		return false
	}
	b := v.(*skewBridge)
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok = b.held[uid]
	return ok
}

// Hold occupies a VM slot and cordons the node. A second hold for the
// same uid does not acquire again. TryAcquire failing still cordons;
// Release then must not free a slot this hold did not take. prior
// unschedulable is false: Recover runs before this process has read
// the API node's spec.
func (b *skewBridge) Hold(uid, reason string) error {
	if b == nil || b.e == nil || uid == "" {
		return nil
	}
	b.mu.Lock()
	if _, ok := b.held[uid]; ok {
		b.mu.Unlock()
		return nil
	}
	b.held[uid] = holdState{reason: reason}
	b.mu.Unlock()
	took := false
	if b.e.slots != nil {
		if err := b.e.slots.TryAcquire(uid); err == nil {
			took = true
		}
	}
	b.mu.Lock()
	st := b.held[uid]
	st.slot = took
	st.reason = reason
	b.held[uid] = st
	b.mu.Unlock()
	b.cordon.Hold(false, reason)
	if b.e.events != nil {
		b.e.events.Warn(context.Background(), node.ReasonSupervisorSkew, reason)
	}
	return nil
}

// Release frees the slot once and, when this process cordoned the node
// and no other hold remains, clears that cordon. A uid that was not held
// is ignored.
func (b *skewBridge) Release(uid string) {
	if b == nil || b.e == nil || uid == "" {
		return
	}
	b.mu.Lock()
	st, ok := b.held[uid]
	if !ok {
		b.mu.Unlock()
		return
	}
	delete(b.held, uid)
	b.mu.Unlock()
	if st.slot && b.e.slots != nil {
		b.e.slots.Release(uid)
	}
	if b.cordon.Release() && b.e.events != nil {
		b.e.events.Normal(context.Background(), "SupervisorVersionSkewCleared", "supervisor version skew cleared")
	}
	b.finishPlaceholder(uid)
}

func (b *skewBridge) reasonFor(uid string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.held[uid].reason
}

func (b *skewBridge) finishPlaceholder(uid string) {
	e := b.e
	e.mu.Lock()
	var rec *podRecord
	for _, r := range e.pods {
		r.mu.Lock()
		match := r.pod != nil && string(r.pod.UID) == uid && r.reason == skewPodReason
		r.mu.Unlock()
		if match {
			rec = r
			break
		}
	}
	e.mu.Unlock()
	if rec == nil {
		return
	}
	rec.mu.Lock()
	if rec.reason != skewPodReason {
		rec.mu.Unlock()
		return
	}
	rec.phase = corev1.PodFailed
	rec.reason = "SupervisorExited"
	rec.message = "supervisor exited; the VM was not stopped by this agent"
	cancel := rec.cancel
	rec.cancel = nil
	rec.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// keepVersionSkew records a placeholder for a pod whose supervisor is
// held. Create must not boot a second VM, and a Never pod must not be
// settled as a node restart while the old VM is still running. The
// placeholder does not own the slot; the hold does.
func (e *Engine) keepVersionSkew(ctx context.Context, pod *corev1.Pod) (bool, error) {
	_ = ctx
	if e == nil || pod == nil {
		return false, nil
	}
	uid := string(pod.UID)
	if !e.skewHolding(uid) {
		return false, nil
	}
	msg := e.skewBridge().reasonFor(uid)
	if msg == "" {
		msg = "supervisor wire major does not match; VM left running"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	key := Key(pod.Namespace, pod.Name)
	if _, exists := e.pods[key]; exists {
		return true, nil
	}
	_, cancel := context.WithCancel(context.Background())
	rec := &podRecord{
		pod:     pod.DeepCopy(),
		phase:   corev1.PodPending,
		reason:  skewPodReason,
		message: msg,
		cancel:  cancel,
	}
	e.pods[key] = rec
	return true, nil
}

type workloadView interface {
	WorkloadStarted() bool
	WorkloadExit() (int32, bool)
}

func supervisorMayStartWorkload(m runtime.Machine) bool {
	v, ok := m.(workloadView)
	if !ok || v == nil {
		return false
	}
	return !v.WorkloadStarted()
}

func supervisorWorkloadResult(m runtime.Machine) (int32, bool) {
	v, ok := m.(workloadView)
	if !ok || v == nil {
		return 0, false
	}
	return v.WorkloadExit()
}

func (e *Engine) applyRecoveredWorkload(ctx context.Context, cli *guest.Client, pod *corev1.Pod, machine runtime.Machine) error {
	if !supervisorMayStartWorkload(machine) {
		return nil
	}
	return e.applyContainerWorkload(ctx, cli, pod)
}

// finishSupervisorWorkload publishes an exit the handshake already
// reported. Code 0 is Succeeded. Any other code is Failed with that
// code. Both paths stop the VM: the workload is finished. A wire-major
// mismatch never reaches this function.
func (e *Engine) finishSupervisorWorkload(rec *podRecord, code int32) {
	if e == nil || rec == nil {
		return
	}
	rec.mu.Lock()
	c := code
	rec.guestExit = &c
	if code == 0 {
		rec.guestExitReason = "Completed"
	} else {
		rec.guestExitReason = "Error"
	}
	rec.mu.Unlock()
	if code == 0 {
		e.succeedWorkload(rec)
		return
	}
	e.failWith(rec, "Error", fmt.Errorf("workload exited %d", code))
}

type workloadNote interface {
	NoteWorkload(context.Context, bool, bool, *int32) error
}

// noteSupervisorWorkload records a successful Workload RPC, or a guest
// exit, on the supervisor handshake. A later agent reads that handshake
// and must not start the command again. The error is ignored: the guest
// already has the result, and a missed note is the crash window the
// guest's own second-claim rejection covers.
func (e *Engine) noteSupervisorWorkload(rec *podRecord, started, exited bool, code *int32) {
	if e == nil || rec == nil {
		return
	}
	rec.mu.Lock()
	m := rec.machine
	rec.mu.Unlock()
	note, ok := m.(workloadNote)
	if !ok || note == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = note.NoteWorkload(ctx, started, exited, code)
}

func (e *Engine) noteSupervisorWorkloadPod(pod *corev1.Pod, started, exited bool, code *int32) {
	if e == nil || pod == nil {
		return
	}
	e.mu.RLock()
	rec := e.pods[Key(pod.Namespace, pod.Name)]
	e.mu.RUnlock()
	e.noteSupervisorWorkload(rec, started, exited, code)
}

func (e *Engine) podWorkloadStarted(pod *corev1.Pod) bool {
	if e == nil || pod == nil {
		return false
	}
	e.mu.RLock()
	rec := e.pods[Key(pod.Namespace, pod.Name)]
	e.mu.RUnlock()
	if rec == nil {
		return false
	}
	rec.mu.Lock()
	m := rec.machine
	rec.mu.Unlock()
	v, ok := m.(workloadView)
	if !ok || v == nil {
		return false
	}
	return v.WorkloadStarted()
}
