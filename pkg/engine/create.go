package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mlvea/darwin-node/internal/netutil"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/types"
	"github.com/mlvea/darwin-node/pkg/volume"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	corev1 "k8s.io/api/core/v1"
)

// Create validates and accepts the pod. The VM slot is acquired in start,
// immediately before Runtime.Create, so an image pull or a boot retry does
// not pin a slot. A pod is rejected here only when every slot is already
// held by a running VM and nothing warm can be reclaimed.
func (e *Engine) Create(ctx context.Context, pod *corev1.Pod, creds Credentials) error {
	if err := ValidatePod(pod, e.cfg.AllowedHostPaths); err != nil {
		e.events.Warn(ctx, event.ReasonFailedCreate, err.Error())
		return errdefs.InvalidInput(err.Error())
	}
	key := Key(pod.Namespace, pod.Name)
	uid := string(pod.UID)

	e.mu.Lock()
	if _, exists := e.pods[key]; exists {
		e.mu.Unlock()
		return nil
	}
	// A supervisor that outlived the previous process is claimed before
	// the node-restart settlement, which would otherwise mark the pod
	// Failed and leave the VM running with no owner.
	started, discard := e.claimRecoveredLocked(pod)
	if discard != nil {
		e.mu.Unlock()
		e.stopSupervisorMachine(context.Background(), discard, true)
		e.mu.Lock()
		if _, exists := e.pods[key]; exists {
			e.mu.Unlock()
			return nil
		}
	}
	if started != nil {
		e.mu.Unlock()
		return e.launchRecovered(started, creds)
	}
	if settled, why, msg := e.settleInterruptedLocked(pod); settled {
		e.mu.Unlock()
		if why != "" {
			e.events.Warn(ctx, why, msg)
		}
		return nil
	}
	if e.draining.Load() {
		e.mu.Unlock()
		e.events.Warn(ctx, event.ReasonDraining, "pod create rejected: node is draining")
		return fmt.Errorf("node is draining; pod will be scheduled elsewhere")
	}
	e.mu.Unlock()

	// Sweep and the slot table take their own locks. Doing this outside
	// e.mu keeps a crashed warm VM's Stop off the pod map.
	if err := e.rejectIfFull(); err != nil {
		e.events.Warn(ctx, event.ReasonVMCapacityExhausted, err.Error())
		return err
	}

	e.mu.Lock()
	if _, exists := e.pods[key]; exists {
		e.mu.Unlock()
		return nil
	}
	if e.draining.Load() {
		e.mu.Unlock()
		e.events.Warn(ctx, event.ReasonDraining, "pod create rejected: node is draining")
		return fmt.Errorf("node is draining; pod will be scheduled elsewhere")
	}
	credit := e.takeRestartCreditLocked(uid)
	pctx, cancel := context.WithCancel(context.Background())
	rec := &podRecord{
		pod:          pod.DeepCopy(),
		phase:        corev1.PodPending,
		reason:       "Starting",
		cancel:       cancel,
		restartCount: credit,
	}
	// Count the goroutine before the pod is visible. Delete waits on
	// startDone before it drops the slot; a Wait that returns because the
	// counter was still zero lets start call Runtime.Create with no slot.
	rec.startDone.Add(1)
	e.pods[key] = rec
	e.mu.Unlock()

	// Freeze the listen address with the reservation. A later interface
	// change must not Bind or Release under a different key.
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
			cancel()
			// Create has not acquired a slot. Release is idempotent.
			e.slots.Release(uid)
			rec.startDone.Done()
			return err
		}
	}

	e.events.Normal(ctx, event.ReasonCreated, "accepted macOS pod; booting VM")
	go func() {
		defer rec.startDone.Done()
		e.start(pctx, rec, creds)
	}()
	return nil
}

func (e *Engine) start(ctx context.Context, rec *podRecord, creds Credentials) {
	rec.mu.Lock()
	pod := rec.pod.DeepCopy()
	rec.mu.Unlock()

	// Warm adoption happens before any slot is taken for a cold boot.
	// A size or credential mismatch leaves the warm VM in the pool.
	entry, aerr := e.tryAdoptWarm(ctx, pod, creds)
	if aerr != nil {
		if ctx.Err() == nil {
			e.fail(rec, aerr)
		}
		return
	}
	if entry != nil {
		e.startAdopted(ctx, rec, pod, creds, entry)
		return
	}

	e.events.Normal(ctx, event.ReasonStarting, "starting macOS VM")

	cpu, mem, err := VMResources(pod.Spec.Containers[0])
	if err != nil {
		e.fail(rec, err)
		return
	}
	mac, err := e.podMAC(string(pod.UID))
	if err != nil {
		e.fail(rec, err)
		return
	}
	token, err := randomToken()
	if err != nil {
		e.fail(rec, err)
		return
	}

	volRoot := filepath.Join(e.cfg.CacheDir, "pods", string(pod.UID), "volumes")
	limits, err := volume.EmptyDirLimits(pod, volRoot)
	if err != nil {
		e.fail(rec, err)
		return
	}
	shares, places, err := volume.Materialize(creds.volumeReq(pod, pod.Spec.Containers[0], volRoot, e.cfg.AllowedHostPaths))
	if err != nil {
		e.fail(rec, err)
		return
	}
	rec.mu.Lock()
	rec.emptyDirs = limits
	rec.volRoot = volRoot
	rec.tokens = append([]volume.ProjectedToken(nil), creds.Tokens...)
	rec.refreshAt = nextTokenRefresh(rec.tokens, time.Now())
	rec.mu.Unlock()
	ctrl := filepath.Join(e.cfg.CacheDir, "pods", string(pod.UID), "control")
	if err := os.MkdirAll(ctrl, 0o700); err != nil {
		e.fail(rec, err)
		return
	}
	if err := os.WriteFile(filepath.Join(ctrl, types.GuestAgentTokenFile), []byte(token), 0o600); err != nil {
		e.fail(rec, err)
		return
	}
	shares = append(shares, types.Share{Name: types.ControlShareName, HostPath: ctrl, ReadOnly: true})

	cacheShares, cachePlaces, err := e.prepareCaches(pod)
	if err != nil {
		e.events.Warn(ctx, event.ReasonFailedCreate, err.Error())
		e.fail(rec, errdefs.InvalidInput(err.Error()))
		return
	}
	shares = append(shares, cacheShares...)
	places = append(places, cachePlaces...)

	if err := e.runInitContainers(ctx, rec, pod, creds, volRoot); err != nil {
		e.fail(rec, err)
		return
	}

	spec := types.VMSpec{
		ID: types.MachineID{
			Namespace: pod.Namespace,
			Name:      pod.Name,
			UID:       string(pod.UID),
		},
		ImageRef:     pod.Spec.Containers[0].Image,
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
		var disk, aux, hw string
		err := e.retryTransient(ctx, rec, "image pull", func() error {
			var perr error
			disk, aux, hw, perr = e.resolveImage(ctx, pod.Spec.Containers[0].Image, string(pod.UID), pod.Spec.Containers[0].ImagePullPolicy, creds)
			return perr
		})
		if err != nil {
			if ctx.Err() == nil {
				e.fail(rec, err)
			}
			return
		}
		spec.DiskPath = disk
		spec.AuxPath = aux
		spec.HardwareModelData = hw
	}

	var machine runtime.Machine
	uid := string(pod.UID)
	if err := e.retryTransient(ctx, rec, "VM boot", func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The slot is taken only for the Create/Start attempt. A failure
		// releases it before retryTransient sleeps, so a pod stuck backing
		// off does not pin half the node.
		if err := e.acquireColdSlot(ctx, uid); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		release := true
		defer func() {
			if release {
				e.stopMachineKeepSlot(rec)
				e.slots.Release(uid)
			}
		}()
		e.stopMachineKeepSlot(rec)
		m, cerr := e.rt.Create(ctx, spec)
		if cerr != nil {
			return cerr
		}
		rec.mu.Lock()
		rec.machine = m
		rec.mu.Unlock()
		if serr := m.Start(ctx); serr != nil {
			return serr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		machine = m
		release = false
		return nil
	}); err != nil {
		if ctx.Err() == nil {
			e.fail(rec, err)
		}
		return
	}
	e.serveConsole(rec)
	e.noteImageRef(pod.Spec.Containers[0].Image)
	e.runConnected(ctx, rec, pod, creds, machine, token, places)
}

// startAdopted hands a pre-booted warm VM to a pod. tryAdoptWarm has
// already moved the slot onto the pod and written a fresh agent token.
func (e *Engine) startAdopted(ctx context.Context, rec *podRecord, pod *corev1.Pod, creds Credentials, entry *warmEntry) {
	rec.mu.Lock()
	rec.machine = entry.machine
	rec.warmDir = entry.root
	rec.warmDisk = entry.diskDir
	rec.mu.Unlock()

	e.events.Normal(ctx, event.ReasonStarting, "adopting pre-booted macOS VM")

	volRoot := filepath.Join(e.cfg.CacheDir, "pods", string(pod.UID), "volumes")
	if len(pod.Spec.InitContainers) > 0 {
		if err := e.runInitContainers(ctx, rec, pod, creds, volRoot); err != nil {
			releaseWarmProbe(entry)
			e.fail(rec, err)
			return
		}
	}
	// The guest agent is still inside Serve on the readiness dial. Close
	// that session now, immediately before the pod dials, so the agent
	// loops and connects again. Closing earlier (while the VM sits idle,
	// or during init) makes the guest time out a handshake nobody is
	// accepting yet.
	releaseWarmProbe(entry)
	e.serveConsole(rec)
	e.runConnected(ctx, rec, pod, creds, entry.machine, entry.token, nil)
}

func podMACDir(cacheDir, uid string) string {
	return filepath.Join(cacheDir, "pods", uid)
}

func loadMAC(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "mac.txt"))
	if err != nil {
		return "", err
	}
	mac := strings.TrimSpace(string(b))
	if _, err := net.ParseMAC(mac); err != nil {
		return "", err
	}
	return mac, nil
}

func persistMAC(dir, mac string) error {
	if _, err := net.ParseMAC(mac); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "mac.txt")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(mac+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (e *Engine) podMAC(uid string) (string, error) {
	dir := podMACDir(e.cfg.CacheDir, uid)
	if mac, err := loadMAC(dir); err == nil {
		return mac, nil
	}
	hw, err := netutil.RandomMAC()
	if err != nil {
		return "", err
	}
	mac := hw.String()
	if err := persistMAC(dir, mac); err != nil {
		return "", err
	}
	return mac, nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
