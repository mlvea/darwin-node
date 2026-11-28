package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/volume"

	corev1 "k8s.io/api/core/v1"
)

// errPodStopping means Delete or fail already owns the pod. The caller must
// not Start the VM or fail it again.
var errPodStopping = errors.New("pod is stopping")

func podActive(rec *podRecord) bool {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return rec.phase == corev1.PodPending || rec.phase == corev1.PodRunning
}

func (e *Engine) restartVM(ctx context.Context, rec *podRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !podActive(rec) {
		return errPodStopping
	}
	rec.mu.Lock()
	machine := rec.machine
	pod := rec.pod.DeepCopy()
	places := append([]volume.Placement(nil), rec.places...)
	oldAgent := rec.agent
	rec.agent = nil
	// The previous generation's exit notice does not belong to the new
	// container. Bump the generation so a late frame from the old agent
	// cannot write it back.
	rec.agentGen++
	rec.guestExit = nil
	rec.guestExitReason = ""
	// The restarted container has not passed its startup probe. Clear it
	// before the new agent is stored so status cannot observe Started.
	// A readiness probe starts not-ready again even when there is no
	// startup probe.
	if pod != nil && len(pod.Spec.Containers) > 0 && hasStartupProbe(pod.Spec.Containers[0]) {
		rec.startupDone = false
		rec.ready = false
		rec.reason = "StartupProbe"
		rec.message = "container restarted"
		rec.startupSince = e.now()
	} else if pod != nil && len(pod.Spec.Containers) > 0 && hasReadinessProbe(pod.Spec.Containers[0]) {
		rec.ready = false
		rec.reason = "ReadinessProbe"
		rec.message = "container restarted"
	}
	rec.mu.Unlock()
	if machine == nil {
		return fmt.Errorf("no machine to restart")
	}
	// The watch goroutine may not have observed VMRunning yet. Its first
	// pass would read the Stop below as an unexpected death.
	markRestarting(rec)
	defer clearRestarting(rec)
	if oldAgent != nil {
		_ = oldAgent.Close() // release the pre-restart vsock/TCP connection
	}
	_ = machine.Stop(ctx, 5*time.Second)
	// Delete may have cancelled while Stop ran. Starting now would boot a
	// VM whose slot Close is about to release.
	if err := ctx.Err(); err != nil {
		return err
	}
	if !podActive(rec) {
		return errPodStopping
	}
	if err := machine.Start(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !podActive(rec) {
		return errPodStopping
	}
	cli, err := machine.DialAgent(ctx)
	if err != nil {
		return err
	}
	// A restarted VM is a new boot. Reseed it before the workload or a
	// copy-mode rematerialize. The caller stops the VM if this fails.
	if err := e.reseedGuest(ctx, cli, pod); err != nil {
		_ = cli.Close()
		return err
	}
	if err := e.applyContainerWorkload(ctx, cli, pod); err != nil {
		_ = cli.Close()
		return err
	}
	rec.mu.Lock()
	rec.agent = cli
	rec.agentOK = true
	rec.mu.Unlock()
	e.watchGuestExit(rec, cli)

	reported := ""
	if netinfo, err := cli.NetInfo(ctx); err == nil {
		reported = netinfo.PrimaryIP
	}
	// A liveness restart learns the address the host assigned. The guest's
	// NetInfo is accepted only when it names that same address, so the
	// proxy cannot be pointed at loopback or another network.
	ip := e.notePodIP(ctx, rec, machine, reported)
	if err := e.syncHostPorts(rec, pod, ip); err != nil {
		return err
	}
	if len(places) > 0 {
		var vp []guest.VolumePlace
		for _, p := range places {
			if p.Mode != "copy" {
				continue
			}
			vp = append(vp, guest.VolumePlace{Name: p.Name, GuestPath: p.GuestPath, ReadOnly: p.ReadOnly, Mode: p.Mode})
		}
		if len(vp) > 0 {
			res, err := cli.Materialize(ctx, guest.MaterializeReq{Volumes: vp})
			if merr := materializeFailure(res, err); merr != nil {
				// rec.agent is already the new dial. The caller fails the
				// pod, and teardown closes it.
				return merr
			}
		}
	}
	e.events.Normal(ctx, event.ReasonVMRestarted, "guest agent reconnected")
	return nil
}
