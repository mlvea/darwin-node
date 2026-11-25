package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"

	corev1 "k8s.io/api/core/v1"
)

// Close rejects new creates, stops the warm pool, and force-stops every pod
// machine still registered. Slots are released. A cache is written back
// only when that workload already exited 0 and the pod is allowed to write
// it; Close of a pod that is still running does not replace the store. A
// second call returns the same error and does not stop anything again.
//
// A Create, Start, Stop, or RemovePod that does not return within the
// deadline does not keep Close waiting. The error is logged and returned.
// The VM slot stays held until that call returns and the normal path
// releases it, so a VM that might still be booting is still accounted
// against the 2-VM cap. Process exit calls FinishShutdown, which waits a
// short extra bound and records whatever is still in flight. Graceful
// shutdown still goes through Drain first; Close is what runs when the
// drain budget is exhausted or draining was skipped.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.draining.Store(true)
		e.closeErr = errors.Join(e.shutdownWarmPool(), e.stopPodMachines())
	})
	return e.closeErr
}

func (e *Engine) closeDeadline() time.Duration {
	if e.closeTimeout > 0 {
		return e.closeTimeout
	}
	return defaultCloseTimeout
}

// callWithin runs fn and returns errCloseDeadline if it is still running
// when the deadline passes. fn is left running: the caller must not release
// a VM slot that fn might still boot. A non-nil boot is recorded when the
// deadline fires. A nil boot records every warm boot still in flight.
// The flight is counted until fn returns, so FinishShutdown can tell a
// late return from a boot that is still going.
func (e *Engine) callWithin(d time.Duration, boot *abandonedBoot, fn func()) error {
	var noted abandonedBoot
	noteOne := boot != nil && boot.ID != ""
	if noteOne {
		noted = *boot
	}
	done := make(chan struct{})
	e.beginFlight()
	go func() {
		defer close(done)
		defer e.endFlight(noted, noteOne)
		fn()
	}()
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-done:
		return nil
	case <-timer.C:
		select {
		case <-done:
			return nil
		default:
			if boot != nil {
				if noteOne {
					e.noteAbandoned(noted)
				}
			} else {
				e.noteInflightWarm()
			}
			return errCloseDeadline
		}
	}
}

var errCloseDeadline = errors.New("close deadline exceeded")

// stopPodMachines force-stops whatever is still registered. Grace is zero.
// When Drain ran, preStop and the guest shutdown RPC already had their turn.
// Pods are stopped together so one stuck call cannot consume the deadline
// before the others are asked to stop.
func (e *Engine) stopPodMachines() error {
	e.mu.RLock()
	victims := make([]drainVictim, 0, len(e.pods))
	for _, rec := range e.pods {
		rec.mu.Lock()
		victims = append(victims, drainVictim{
			ns:   rec.pod.Namespace,
			name: rec.pod.Name,
			uid:  string(rec.pod.UID),
		})
		rec.mu.Unlock()
	}
	e.mu.RUnlock()
	sort.Slice(victims, func(i, j int) bool {
		if victims[i].ns != victims[j].ns {
			return victims[i].ns < victims[j].ns
		}
		return victims[i].name < victims[j].name
	})
	d := e.closeDeadline()
	var (
		mu   sync.Mutex
		errs []error
		wg   sync.WaitGroup
	)
	for _, v := range victims {
		wg.Add(1)
		go func(v drainVictim) {
			defer wg.Done()
			boot := abandonedBoot{ID: v.uid, Kind: abandonedPodKind, Ref: v.ns + "/" + v.name}
			if err := e.callWithin(d, &boot, func() {
				_ = e.Delete(context.Background(), v.ns, v.name, 0)
			}); err == nil {
				return
			}
			msg := fmt.Sprintf("close: pod %s/%s still stopping after %s; VM slot kept", v.ns, v.name, d)
			slog.Error(msg)
			e.events.Warn(context.Background(), event.ReasonFailed, msg)
			mu.Lock()
			errs = append(errs, errors.New(msg))
			mu.Unlock()
		}(v)
	}
	wg.Wait()
	return errors.Join(errs...)
}

// applyStopPhaseLocked records the phase Delete publishes while the pod is
// still in the provider. A guest exit of 0 already finished the workload.
// Anything else that has not reached a terminal phase was stopped first:
// grace 0 is SIGKILL (137) and a positive grace is SIGTERM (143).
func applyStopPhaseLocked(rec *podRecord, grace int64) {
	if rec.phase == corev1.PodFailed || rec.phase == corev1.PodSucceeded {
		return
	}
	if rec.guestExit != nil && *rec.guestExit == 0 {
		rec.phase = corev1.PodSucceeded
		if rec.guestExitReason != "" {
			rec.reason = rec.guestExitReason
		} else {
			rec.reason = "Completed"
		}
		rec.message = "workload exited 0"
		return
	}
	if rec.guestExit != nil {
		rec.phase = corev1.PodFailed
		if rec.guestExitReason != "" {
			rec.reason = rec.guestExitReason
		} else if rec.reason == "" {
			rec.reason = "Error"
		}
		if rec.message == "" {
			rec.message = fmt.Sprintf("workload exited %d", *rec.guestExit)
		}
		return
	}
	code := int32(143)
	msg := "terminated with SIGTERM before the workload exited"
	if grace <= 0 {
		code = 137
		msg = "killed with SIGKILL before the workload exited"
	}
	rec.killedCode = &code
	rec.phase = corev1.PodFailed
	rec.reason = "Error"
	rec.message = msg
}

func (e *Engine) teardown(ctx context.Context, rec *podRecord, grace int64, runHooks bool) {
	rec.mu.Lock()
	if rec.cancel != nil {
		rec.cancel()
		rec.cancel = nil
	}
	pod := rec.pod
	machine := rec.machine
	agent := rec.agent
	warmDir := rec.warmDir
	warmDisk := rec.warmDisk
	rec.agent = nil
	rec.ready = false
	consoleLn := rec.consoleLn
	consoleSock := rec.consoleSock
	consoleHub := rec.consoleHub
	rec.consoleLn = nil
	rec.consoleSock = ""
	rec.consoleHub = nil
	uid := ""
	ns, name := "", ""
	if pod != nil {
		uid = string(pod.UID)
		ns, name = pod.Namespace, pod.Name
	}
	rec.mu.Unlock()

	key := Key(ns, name)
	e.hostports.Release(key)
	if consoleHub != nil {
		consoleHub.close()
	}
	if consoleLn != nil {
		_ = consoleLn.Close()
	}
	if consoleSock != "" {
		_ = os.Remove(consoleSock)
	}
	if runHooks && agent != nil && grace > 0 && pod != nil && len(pod.Spec.Containers) > 0 {
		sctx, cancel := context.WithTimeout(ctx, time.Duration(grace)*time.Second)
		if lc := pod.Spec.Containers[0].Lifecycle; lc != nil && lc.PreStop != nil && lc.PreStop.Exec != nil {
			rec.mu.Lock()
			rec.agent = agent
			rec.mu.Unlock()
			if err := e.runHook(sctx, rec, lc.PreStop.Exec.Command, "preStop"); err != nil {
				e.events.Warn(ctx, event.ReasonPreStopFailed, err.Error())
			}
			rec.mu.Lock()
			rec.agent = nil
			rec.mu.Unlock()
		}
		_ = agent.Shutdown(sctx, guest.ShutdownReq{Reason: "pod deleted"})
		cancel()
	}
	// Always close the agent so Session.readLoop leaves ReadFrame. Skipping
	// this left TCP/vsock clients parked until the peer idle-timed out —
	// visible as a leakcheck flake under Darwin CI merge-ref timing.
	if agent != nil {
		_ = agent.Close()
	}
	if machine != nil {
		_ = machine.Stop(ctx, time.Duration(grace)*time.Second)
	}
	_ = e.sidecar.RemovePod(ctx, ns, name, grace)
	if warmDir != "" {
		// Adopted VM: control files live under cache/warm, the CoW disk under
		// cache/pods/<warm-slot>. Both must go or every adoption leaks a disk.
		_ = os.RemoveAll(warmDir)
	}
	if warmDisk != "" {
		_ = os.RemoveAll(warmDisk)
	}
	if uid != "" {
		e.slots.Release(uid)
	}
}

// Draining reports whether Drain has started. New creates are rejected
// while draining.
func (e *Engine) Draining() bool { return e.draining.Load() }

type drainVictim struct {
	ns, name, uid string
	grace         int64
}

// Drain stops every pod gracefully, honoring each pod's termination grace
// period until the context budget is exhausted. Pods that do not finish
// within the budget are left running; a subsequent drain (or process exit)
// handles them. A cache is written back only when that workload already
// exited 0 and the pod is allowed to write it. Drain of a pod that is
// still running does not replace the store.
func (e *Engine) Drain(ctx context.Context) error {
	e.draining.Store(true)
	e.mu.RLock()
	victims := make([]drainVictim, 0, len(e.pods))
	for _, rec := range e.pods {
		rec.mu.Lock()
		grace := int64(defaultGracePeriod / time.Second)
		if rec.pod != nil && rec.pod.Spec.TerminationGracePeriodSeconds != nil {
			grace = *rec.pod.Spec.TerminationGracePeriodSeconds
		}
		if grace < 0 {
			grace = 0
		}
		victims = append(victims, drainVictim{
			ns:    rec.pod.Namespace,
			name:  rec.pod.Name,
			grace: grace,
		})
		rec.mu.Unlock()
	}
	e.mu.RUnlock()
	sort.Slice(victims, func(i, j int) bool {
		if victims[i].ns != victims[j].ns {
			return victims[i].ns < victims[j].ns
		}
		return victims[i].name < victims[j].name
	})

	e.events.Normal(ctx, event.ReasonDraining,
		fmt.Sprintf("draining %d pods", len(victims)))
	var firstErr error
	for i, v := range victims {
		if err := ctx.Err(); err != nil {
			e.events.Warn(ctx, event.ReasonDraining,
				fmt.Sprintf("drain budget exhausted; %d pods remain", len(victims)-i))
			return firstErr
		}
		if err := e.Delete(ctx, v.ns, v.name, v.grace); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

const defaultGracePeriod = 30 * time.Second

// Delete stops the VM and releases the slot.
func (e *Engine) Delete(ctx context.Context, namespace, name string, grace int64) error {
	key := Key(namespace, name)
	e.mu.Lock()
	rec, ok := e.pods[key]
	e.mu.Unlock()
	if !ok {
		return nil
	}
	rec.mu.Lock()
	// A terminal phase is kept. Rewriting Failed as Succeeded reported
	// exit 0 for the whole teardown, and rewriting Succeeded as a kill
	// counted a finished Job as a failure. A pod that is still running
	// is Failed: the workload has not exited, so a Job must not count it.
	applyStopPhaseLocked(rec, grace)
	rec.ready = false
	uid := string(rec.pod.UID)
	// Cancel before waiting so a start blocked in Create, Start, or Dial
	// can return. The slot stays held across that wait: teardown used to
	// release it while start was still inside Runtime.Create, and the next
	// two pods then booted a third VM.
	if rec.cancel != nil {
		rec.cancel()
		rec.cancel = nil
	}
	rec.mu.Unlock()

	rec.startDone.Wait()
	// The watch is not part of start. It restarts sidecars and, on a
	// liveness failure, the VM. Wait until that goroutine has left Create
	// and Start before RemovePod and the slot release. Those calls do not
	// observe cancellation once they are in flight (vz.Start ignores ctx).
	rec.watchDone.Wait()

	e.events.Normal(ctx, event.ReasonKilling, "stopping macOS VM")
	e.teardown(ctx, rec, grace, true) // preStop, Shutdown, Stop — RemoveAll only after Stop returns
	// Exit 0 commits. A kill or a non-zero exit leaves the previous store.
	// The pod directory is removed only after that decision.
	e.snapshotPodCaches(rec)
	_ = os.RemoveAll(filepath.Join(e.cfg.CacheDir, "pods", uid))

	// The watch has returned, so it cannot arm the backoff again. Leaving
	// the entry would keep a deleted pod's delay for the life of the
	// process, and a new pod with the same UID would wait it out.
	rec.mu.Lock()
	pod := rec.pod
	rec.mu.Unlock()
	e.dropSidecarBackoff(pod)

	e.mu.Lock()
	delete(e.pods, key)
	e.mu.Unlock()
	return nil
}

// dropSidecarBackoff removes crash-loop entries for this pod. client-go's
// GC only drops an entry after it has been stable for twice the cap, so a
// Delete has to remove its own keys.
func (e *Engine) dropSidecarBackoff(pod *corev1.Pod) {
	if e.sidecarBackOff == nil || pod == nil || pod.UID == "" {
		return
	}
	uid := string(pod.UID)
	for _, c := range pod.Spec.InitContainers {
		e.sidecarBackOff.DeleteEntry(uid + "/" + c.Name)
	}
	for _, c := range pod.Spec.Containers {
		e.sidecarBackOff.DeleteEntry(uid + "/" + c.Name)
	}
}
