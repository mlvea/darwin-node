package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/volume"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// runConnected dials the running machine's agent and drives everything from
// handshake to Running: volume placement, IP, hooks, sidecars, watch loop.
func (e *Engine) runConnected(ctx context.Context, rec *podRecord, pod *corev1.Pod, creds Credentials, machine runtime.Machine, token string, places []volume.Placement) {
	volRoot := filepath.Join(e.cfg.CacheDir, "pods", string(pod.UID), "volumes")
	e.events.Normal(ctx, event.ReasonVMDialing, "dialing guest agent")
	// The guest agent is reached only over vsock. A refused dial is
	// retried. The budget is AgentReadyTimeout, not a fixed 45s.
	cli, err := e.dialGuest(ctx, rec, machine)
	if err != nil {
		if ctx.Err() == nil {
			e.fail(rec, err)
		}
		return
	}
	if len(places) > 0 {
		var vp []guest.VolumePlace
		for _, p := range places {
			vp = append(vp, guest.VolumePlace{Name: p.Name, GuestPath: p.GuestPath, ReadOnly: p.ReadOnly, Mode: p.Mode})
		}
		res, err := cli.Materialize(ctx, guest.MaterializeReq{Volumes: vp})
		if merr := materializeFailure(res, err); merr != nil {
			// rec.agent is still nil, so teardown will not close this session.
			_ = cli.Close()
			e.fail(rec, merr)
			return
		}
	}

	reported := ""
	if netinfo, err := cli.NetInfo(ctx); err == nil {
		reported = netinfo.PrimaryIP
	}
	e.notePodIP(ctx, rec, machine, reported)

	if err := e.applyContainerWorkload(ctx, cli, pod); err != nil {
		_ = cli.Close()
		e.fail(rec, err)
		return
	}

	rec.mu.Lock()
	rec.machine = machine
	rec.agent = cli
	rec.token = token
	rec.mu.Unlock()
	e.watchGuestExit(rec, cli)

	if lc := pod.Spec.Containers[0].Lifecycle; lc != nil && lc.PostStart != nil && lc.PostStart.Exec != nil {
		if err := e.runHook(ctx, rec, lc.PostStart.Exec.Command, "postStart"); err != nil {
			e.events.Warn(ctx, event.ReasonPostStartFailed, err.Error())
			e.fail(rec, err)
			return
		}
	}

	for i := 1; i < len(pod.Spec.Containers); i++ {
		if _, _, err := volume.Materialize(creds.volumeReq(pod, pod.Spec.Containers[i], volRoot, e.cfg.AllowedHostPaths)); err != nil {
			e.fail(rec, err)
			return
		}
		if err := e.sidecar.Create(sidecar.WithCreateOptions(ctx, e.sidecarCreateOptions(creds)), pod, pod.Spec.Containers[i], volRoot); err != nil {
			e.fail(rec, err)
			return
		}
	}
	e.refreshSidecarStatus(ctx, rec)
	if !e.noteSidecarExit(rec) {
		return
	}

	now := metav1.Now()
	rec.mu.Lock()
	rec.machine = machine
	rec.agent = cli
	rec.places = places
	rec.token = token
	rec.phase = corev1.PodRunning
	rec.reason = ""
	rec.message = ""
	rec.startedAt = &now
	rec.agentOK = true
	rec.startupDone = !hasStartupProbe(pod.Spec.Containers[0])
	// A readiness probe starts not-ready. One sample must not flip it;
	// successThreshold successes do, and it stays ready until
	// failureThreshold failures.
	_, hasReadyProbe := k8sProbe(pod.Spec.Containers[0].ReadinessProbe, pod.Spec.Containers[0].Ports)
	rec.ready = rec.startupDone && !hasReadyProbe
	ip := rec.pod.Status.PodIP
	rec.mu.Unlock()
	if err := e.syncHostPorts(rec, pod, ip); err != nil {
		e.events.Warn(ctx, event.ReasonFailed, "hostPort: "+err.Error())
		e.fail(rec, err)
		return
	}
	e.events.Normal(ctx, event.ReasonStarted, "macOS VM running; guest agent ready")
	// Count the watch before it runs. Delete waits for it, then stops the
	// VM and removes sidecars. A restart already inside Start or Create
	// would otherwise finish after the slot was released.
	rec.watchDone.Add(1)
	go func() {
		defer rec.watchDone.Done()
		e.watch(ctx, rec)
	}()
}

// watchGuestExit records Exit notifies from this client on the pod. A
// restart bumps the generation, which drops a late frame from the client
// that just stopped and clears the code so the new container does not
// inherit it.
func (e *Engine) watchGuestExit(rec *podRecord, cli *guest.Client) {
	if e == nil || rec == nil || cli == nil {
		return
	}
	rec.mu.Lock()
	rec.agentGen++
	gen := rec.agentGen
	rec.guestExit = nil
	rec.guestExitReason = ""
	rec.mu.Unlock()
	cli.OnExit(func(rep guest.ExitReport) {
		if !rep.Reported() {
			return
		}
		code := guest.ClampExitCode(*rep.ExitCode)
		reason := guest.NormalizeExitReason(rep.Reason)
		rec.mu.Lock()
		if rec.agentGen != gen {
			rec.mu.Unlock()
			return
		}
		rec.guestExit = &code
		rec.guestExitReason = reason
		killed := rec.killedCode != nil
		failed := rec.phase == corev1.PodFailed
		rec.mu.Unlock()
		// The workload has ended. Commit before the VM is torn down and
		// before anything removes the pod directory. A kill that already
		// won, or a non-zero exit, does not replace a good cache.
		if code == 0 && !killed && !failed {
			e.snapshotPodCaches(rec)
		}
	})
}

func (e *Engine) fail(rec *podRecord, err error) {
	e.failWith(rec, "Failed", err)
}

func (e *Engine) failWith(rec *podRecord, reason string, err error) {
	rec.mu.Lock()
	// Failed or Succeeded means Delete or a workload exit already owns the
	// pod. A watch tick that observed Stopped must not tear it down twice.
	if rec.phase == corev1.PodFailed || rec.phase == corev1.PodSucceeded {
		rec.mu.Unlock()
		return
	}
	if reason == "" {
		reason = "Failed"
	}
	rec.phase = corev1.PodFailed
	rec.reason = reason
	rec.message = guest.SanitizeGuestText(err.Error())
	rec.failedErr = err
	rec.ready = false
	uid := ""
	if rec.pod != nil {
		uid = string(rec.pod.UID)
	}
	rec.mu.Unlock()
	e.teardown(context.Background(), rec, 0, false)
	// A failed pod's overlay can be multiple gigabytes; nothing will ever
	// restart it, so reclaim the directory now instead of leaving it for
	// the next process's startup sweep.
	if uid != "" && !e.slots.Holds(uid) {
		_ = os.RemoveAll(filepath.Join(e.cfg.CacheDir, "pods", uid))
	}
	e.events.Warn(context.Background(), event.ReasonFailed, guest.SanitizeGuestText(err.Error()))
}

// succeedWorkload marks a Running pod Succeeded after the guest reported
// exit 0 and the restart policy will not start it again. The slot is
// released. The cache commit (clone into a temp dir, then rename onto the
// store) finishes before Succeeded is visible and before teardown, while
// the pod directory is still present. A waiter that observes Succeeded
// therefore observes the committed store. Delete removes that directory
// later; a process restart would otherwise reap it without a snapshot.
func (e *Engine) succeedWorkload(rec *podRecord) {
	rec.mu.Lock()
	if rec.phase != corev1.PodRunning && rec.phase != corev1.PodPending {
		rec.mu.Unlock()
		return
	}
	rec.mu.Unlock()
	// Publishing Succeeded before this returns let readers observe success
	// while the rename was still in flight and the store file was missing.
	e.snapshotPodCaches(rec)
	rec.mu.Lock()
	if rec.phase != corev1.PodRunning && rec.phase != corev1.PodPending {
		rec.mu.Unlock()
		return
	}
	reason := "Completed"
	if rec.guestExitReason != "" {
		reason = rec.guestExitReason
	}
	rec.phase = corev1.PodSucceeded
	rec.reason = reason
	rec.message = "workload exited 0"
	rec.ready = false
	rec.mu.Unlock()
	e.teardown(context.Background(), rec, 0, false)
	e.events.Normal(context.Background(), event.ReasonCompleted, "workload exited 0")
}

func materializeFailure(res guest.MaterializeRes, err error) error {
	if err != nil {
		return fmt.Errorf("materialize: %s", guest.SanitizeGuestText(err.Error()))
	}
	if res.OK {
		return nil
	}
	msg := guest.SanitizeGuestText(strings.TrimSpace(res.Message))
	if msg == "" {
		msg = "guest refused volume placement"
	}
	return fmt.Errorf("materialize: %s", msg)
}
