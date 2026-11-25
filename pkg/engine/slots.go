package engine

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/image"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// agentTokenSetter is implemented by runtimes whose guest does not watch
// the control-share token file. The vz guest does watch it. The fake
// guest used in tests does not, so adoption calls this when present.
type agentTokenSetter interface {
	SetAgentToken(string)
}

// rejectIfFull refuses a pod only when every slot is held and there is no
// warm VM to reclaim and no warm boot to preempt. Image pull and boot
// backoff do not hold a slot, so this is the synchronous backstop for pods
// that the scheduler should already have kept off a full node.
func (e *Engine) rejectIfFull() error {
	e.sweepCrashedWarm()
	if e.slots.Free() > 0 || e.warm.len() > 0 || e.warmBooting() > 0 {
		return nil
	}
	return e.capacityError()
}

func (e *Engine) capacityError() error {
	return fmt.Errorf("rejected: %s capacity exhausted (%d/%d in use); this pod was not queued: %w",
		types.ResourceVMSlot, e.slots.Used(), e.slots.Max(), capacity.ErrVMCapacityExhausted)
}

// acquireColdSlot takes a slot for a cold boot. A free slot is used as-is.
// Otherwise the oldest warm VM is evicted, or one in-flight warm boot is
// cancelled. The caller releases the slot before any retry sleep.
func (e *Engine) acquireColdSlot(ctx context.Context, uid string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.slots.Holds(uid) {
		return nil
	}
	e.sweepCrashedWarm()
	if err := e.slots.TryAcquire(uid); err == nil {
		return nil
	}
	if entry := e.warm.popOldest(); entry != nil {
		id := entry.slotID
		slog.Info("warm pool: evicting for pod demand", "slot", id)
		e.stopWarmEntry(entry)
		e.events.Normal(context.Background(), event.ReasonWarmEvicted,
			"reclaimed slot "+id+" for a pending pod")
		if err := e.slots.TryAcquire(uid); err == nil {
			return nil
		}
	}
	if e.preemptOneWarmBoot() {
		if err := ctx.Err(); err != nil {
			return err
		}
		e.sweepCrashedWarm()
		if err := e.slots.TryAcquire(uid); err == nil {
			return nil
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return e.capacityError()
}

// tryAdoptWarm returns a pooled VM whose image and size match the pod,
// after the pod's own pull credentials have been checked and the guest
// token has been rotated. A mismatch returns a nil entry and a nil error
// so the caller cold-boots. A token rotation failure stops the VM.
func (e *Engine) tryAdoptWarm(ctx context.Context, pod *corev1.Pod, creds Credentials) (*warmEntry, error) {
	if !canAdopt(pod) {
		return nil, nil
	}
	ref := pod.Spec.Containers[0].Image
	cpu, mem, err := VMResources(pod.Spec.Containers[0])
	if err != nil {
		return nil, nil
	}
	if !e.warm.hasSized(ref, cpu, mem) {
		return nil, nil
	}
	if err := e.authorizeWarmAdopt(ctx, ref, creds); err != nil {
		slog.Warn("warm pool: pod is not allowed to adopt this image; cold boot", "image", ref, "err", err)
		return nil, nil
	}
	entry := e.warm.takeSized(ref, cpu, mem)
	if entry == nil {
		return nil, nil
	}
	uid := string(pod.UID)
	if err := e.slots.Transfer(entry.slotID, uid); err != nil {
		e.stopWarmEntry(entry)
		return nil, nil
	}
	if _, err := e.rotateWarmToken(entry); err != nil {
		// The slot now belongs to the pod. stopWarmEntry releases the
		// warm id, which was already given away.
		e.slots.Release(uid)
		entry.slotID = ""
		e.stopWarmEntry(entry)
		return nil, err
	}
	e.events.Normal(context.Background(), event.ReasonWarmAdopted,
		fmt.Sprintf("pod adopted pre-booted VM %s (no cold boot)", entry.slotID))
	return entry, nil
}

// authorizeWarmAdopt applies the cache-authorization rule to a warm VM.
// A host path is allowed outside cluster mode. A registry image with no
// committed pull cache is not checked here: the fake runtime's warm boots
// have no registry blob, and a cold boot will pull with the pod's creds.
// When a committed cache exists, the pod's own credentials must authorize
// it. Anonymous success means the image is public. Any failure, including
// an unreachable registry, refuses adoption.
func (e *Engine) authorizeWarmAdopt(ctx context.Context, ref string, creds Credentials) error {
	if hostImagePath(ref) {
		if e.cfg.Cluster {
			return fmt.Errorf("load image %q: local image paths are not allowed in cluster mode", ref)
		}
		return nil
	}
	if _, err := image.LoadCommitted(image.CacheDir(e.cfg.CacheDir, ref)); err != nil {
		return nil
	}
	return e.authorizeCached(ctx, ref, creds.Pull)
}

func (e *Engine) rotateWarmToken(entry *warmEntry) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	path := filepath.Join(entry.root, "control", types.GuestAgentTokenFile)
	if err := writeTokenFile(path, token); err != nil {
		return "", err
	}
	entry.token = token
	if setter, ok := entry.machine.(agentTokenSetter); ok {
		setter.SetAgentToken(token)
	}
	return token, nil
}

func writeTokenFile(path, token string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".agent-token-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.WriteString(token); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
