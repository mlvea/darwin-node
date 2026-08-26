// Package engine is the pod lifecycle state machine.
package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darwin-node/darwin-node/internal/netutil"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/hostport"
	"github.com/darwin-node/darwin-node/pkg/image"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"
	"github.com/darwin-node/darwin-node/pkg/volume"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Key is namespace/name.
func Key(ns, name string) string { return ns + "/" + name }

// Credentials are volume-related secrets resolved by the provider.
type Credentials struct {
	ConfigMaps   map[string]*corev1.ConfigMap
	Secrets      map[string]*corev1.Secret
	ServiceToken string
	Pull         image.RegistryCreds
}

// Engine owns in-memory pod state and drives the runtime.
type Engine struct {
	cfg       config.Config
	slots     *capacity.Slots
	rt        runtime.Runtime
	sidecar   sidecar.Runtime
	events    event.Recorder
	hostIP    string
	puller    image.Puller
	hostports *hostport.Manager

	warm       warmPool
	warmCancel context.CancelFunc
	warmCtx    context.Context
	warmBootWG sync.WaitGroup
	lastImage  atomic.Value // string: most recently booted image ref

	mu   sync.RWMutex
	pods map[string]*podRecord
}

type podRecord struct {
	mu sync.Mutex

	pod             *corev1.Pod
	machine         runtime.Machine
	agent           *guest.Client
	places          []volume.Placement
	token           string
	phase           corev1.PodPhase
	reason          string
	message         string
	startedAt       *metav1.Time
	agentOK         bool
	ready           bool
	failedErr       error
	cancel          context.CancelFunc
	restartCount    int32
	initialized     bool
	sidecarStatuses []sidecar.Status
	warmDir         string       // set when the VM was adopted from the warm pool
	adopt           *warmEntry   // claimed under capacity pressure, adopted in start()
	consoleLn       net.Listener // per-VM unix socket for break-glass console
}

// New constructs an engine.
func New(cfg config.Config, slots *capacity.Slots, rt runtime.Runtime, sc sidecar.Runtime, rec event.Recorder, hostIP string) *Engine {
	if rec == nil {
		rec = event.Nop{}
	}
	if sc == nil {
		sc = sidecar.None{}
	}
	puller := image.NewManager(cfg.CacheDir)
	puller.InsecureRegistries = cfg.InsecureRegistries
	e := &Engine{
		cfg:       cfg,
		slots:     slots,
		rt:        rt,
		sidecar:   sc,
		events:    rec,
		hostIP:    hostIP,
		puller:    puller,
		hostports: hostport.New(),
		pods:      map[string]*podRecord{},
	}
	_ = e.Recover()
	if cfg.WarmSlots > 0 {
		e.startWarmPool()
	}
	return e
}

// Close stops the warm pool and every VM it holds. Pod machines keep running.
func (e *Engine) Close() {
	e.shutdownWarmPool()
}

func (e *Engine) Slots() *capacity.Slots { return e.slots }

// Create validates, fail-closed acquires a VM slot, and starts asynchronously.
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
	var adopt *warmEntry
	acqErr := e.slots.TryAcquire(uid)
	if errors.Is(acqErr, capacity.ErrVMCapacityExhausted) {
		// Real demand reclaims warm capacity before any pod is rejected.
		entry, freed := e.reclaimWarmForPod(pod.Spec.Containers[0].Image, canAdopt(pod))
		if freed {
			if entry != nil {
				e.slots.Release(entry.slotID)
			}
			if acqErr = e.slots.TryAcquire(uid); acqErr != nil && entry != nil {
				// Slot raced away; the claimed VM cannot be adopted.
				_ = entry.machine.Stop(context.Background(), 5*time.Second)
				_ = os.RemoveAll(entry.root)
				entry = nil
			} else if acqErr == nil {
				adopt = entry
			}
		}
	}
	if acqErr != nil {
		e.mu.Unlock()
		if errors.Is(acqErr, capacity.ErrVMCapacityExhausted) {
			e.events.Warn(ctx, event.ReasonVMCapacityExhausted, acqErr.Error())
			return acqErr
		}
		return acqErr
	}
	pctx, cancel := context.WithCancel(context.Background())
	rec := &podRecord{
		pod:    pod.DeepCopy(),
		phase:  corev1.PodPending,
		reason: "Starting",
		cancel: cancel,
		adopt:  adopt,
	}
	e.pods[key] = rec
	e.mu.Unlock()

	if maps := hostPortReservations(pod); len(maps) > 0 {
		if err := e.hostports.Reserve(key, maps); err != nil {
			e.mu.Lock()
			delete(e.pods, key)
			e.mu.Unlock()
			cancel()
			e.slots.Release(uid)
			return err
		}
	}

	e.events.Normal(ctx, event.ReasonCreated, "accepted macOS pod; booting VM")
	go e.start(pctx, rec, creds)
	return nil
}

func (e *Engine) start(ctx context.Context, rec *podRecord, creds Credentials) {
	rec.mu.Lock()
	pod := rec.pod.DeepCopy()
	rec.mu.Unlock()

	// Warm adoption: virtio-fs shares are fixed at VM creation, so only pods
	// whose primary container mounts nothing (and declares no caches) can
	// adopt a pre-booted VM. rec.adopt was claimed in Create under capacity
	// pressure; otherwise try the pool directly.
	rec.mu.Lock()
	entry := rec.adopt
	rec.adopt = nil
	rec.mu.Unlock()
	if entry == nil && canAdopt(pod) {
		entry = e.takeWarm(pod.Spec.Containers[0].Image)
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
	shares, places, err := volume.Materialize(volume.Request{
		Pod:              pod,
		Container:        pod.Spec.Containers[0],
		RootDir:          volRoot,
		ConfigMaps:       creds.ConfigMaps,
		Secrets:          creds.Secrets,
		ServiceToken:     creds.ServiceToken,
		AllowedHostPaths: e.cfg.AllowedHostPaths,
	})
	if err != nil {
		e.fail(rec, err)
		return
	}
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
		disk, aux, hw, err := e.resolveImage(ctx, pod.Spec.Containers[0].Image, string(pod.UID), pod.Spec.Containers[0].ImagePullPolicy, creds)
		if err != nil {
			e.fail(rec, err)
			return
		}
		spec.DiskPath = disk
		spec.AuxPath = aux
		spec.HardwareModelData = hw
	}

	machine, err := e.rt.Create(ctx, spec)
	if err != nil {
		e.fail(rec, err)
		return
	}
	rec.mu.Lock()
	rec.machine = machine
	rec.mu.Unlock()
	if err := machine.Start(ctx); err != nil {
		e.fail(rec, err)
		return
	}
	e.serveConsole(rec)
	e.noteImageRef(pod.Spec.Containers[0].Image)
	e.runConnected(ctx, rec, pod, creds, machine, token, places)
}

// startAdopted hands a pre-booted warm VM to a pod. The guest is already
// running and agent-ready; the pod keeps the token it was booted with.
func (e *Engine) startAdopted(ctx context.Context, rec *podRecord, pod *corev1.Pod, creds Credentials, entry *warmEntry) {
	rec.mu.Lock()
	rec.machine = entry.machine
	rec.warmDir = entry.root
	rec.mu.Unlock()

	e.events.Normal(ctx, event.ReasonStarting, "adopting pre-booted macOS VM")

	volRoot := filepath.Join(e.cfg.CacheDir, "pods", string(pod.UID), "volumes")
	if len(pod.Spec.InitContainers) > 0 {
		if err := e.runInitContainers(ctx, rec, pod, creds, volRoot); err != nil {
			e.fail(rec, err)
			return
		}
	}
	e.serveConsole(rec)
	e.runConnected(ctx, rec, pod, creds, entry.machine, entry.token, nil)
}

// serveConsole exposes the pod's serial console on a host-local unix socket:
// <cache-dir>/pods/<uid>/console.sock. `darwin-node console` dials it. The
// socket exists only when --serial-console is enabled and the runtime
// supports it; everything here is best-effort.
func (e *Engine) serveConsole(rec *podRecord) {
	if !e.cfg.SerialConsole {
		return
	}
	rec.mu.Lock()
	machine := rec.machine
	pod := rec.pod
	rec.mu.Unlock()
	if machine == nil || pod == nil {
		return
	}
	cons, ok := machine.(runtime.Consoler)
	if !ok {
		return
	}
	consoleConn, err := cons.Console()
	if err != nil {
		e.events.Warn(context.Background(), event.ReasonFailed, "console: "+err.Error())
		return
	}
	sockPath := ConsoleSocketPath(pod.Namespace + "@" + pod.Name)
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		e.events.Warn(context.Background(), event.ReasonFailed, "console socket: "+err.Error())
		return
	}
	rec.mu.Lock()
	rec.consoleLn = ln
	rec.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go bridgeConsole(consoleConn, c)
		}
	}()
}

// ConsoleSocketPath is where a pod VM's break-glass console is served.
// Deterministic per pod UID so the CLI resolves it without talking to the
// node. Lives in TempDir because unix socket paths are limited to 104 bytes
// on macOS and cache dirs nest deeply.
func ConsoleSocketPath(uid string) string {
	sum := sha256.Sum256([]byte(uid))
	return filepath.Join(os.TempDir(), fmt.Sprintf("darwin-console-%x.sock", sum[:8]))
}

// bridgeConsole copies between the serial stream and one attached client
// until either side closes. Concurrent attachments share the console bytes.
func bridgeConsole(consoleConn io.ReadWriteCloser, client net.Conn) {
	defer client.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(client, consoleConn)
	}()
	_, _ = io.Copy(consoleConn, client)
	<-done
}

// runConnected dials the running machine's agent and drives everything from
// handshake to Running: volume placement, IP, hooks, sidecars, watch loop.
func (e *Engine) runConnected(ctx context.Context, rec *podRecord, pod *corev1.Pod, creds Credentials, machine runtime.Machine, token string, places []volume.Placement) {
	volRoot := filepath.Join(e.cfg.CacheDir, "pods", string(pod.UID), "volumes")
	e.events.Normal(ctx, event.ReasonVMDialing, "dialing guest agent")
	dialCtx := ctx
	cancelDial := func() {}
	if _, ok := ctx.Deadline(); !ok {
		dialCtx, cancelDial = context.WithTimeout(ctx, agentDialTimeout)
	}
	cli, err := machine.DialAgent(dialCtx)
	if err != nil {
		cli, err = e.dialFallback(dialCtx, token, machine)
	}
	cancelDial()
	if err != nil {
		e.fail(rec, fmt.Errorf("guest agent: %w", err))
		return
	}
	readyCtx, cancel := context.WithTimeout(ctx, e.cfg.AgentReadyTimeout)
	defer cancel()
	ready, err := cli.Ready(readyCtx)
	if err != nil || !ready.Ready {
		e.fail(rec, fmt.Errorf("guest agent not ready: %v %w", ready, err))
		return
	}
	if len(places) > 0 {
		var vp []guest.VolumePlace
		for _, p := range places {
			vp = append(vp, guest.VolumePlace{Name: p.Name, GuestPath: p.GuestPath, ReadOnly: p.ReadOnly, Mode: p.Mode})
		}
		if _, err := cli.Materialize(ctx, guest.MaterializeReq{Volumes: vp}); err != nil {
			e.events.Warn(ctx, event.ReasonGuestAgent, "materialize: "+err.Error())
		}
	}

	if netinfo, err := cli.NetInfo(ctx); err == nil && netinfo.PrimaryIP != "" {
		rec.mu.Lock()
		rec.pod.Status.PodIP = netinfo.PrimaryIP
		rec.mu.Unlock()
	}

	rec.mu.Lock()
	rec.machine = machine
	rec.agent = cli
	rec.token = token
	rec.mu.Unlock()

	if lc := pod.Spec.Containers[0].Lifecycle; lc != nil && lc.PostStart != nil && lc.PostStart.Exec != nil {
		if err := e.runHook(ctx, rec, lc.PostStart.Exec.Command, "postStart"); err != nil {
			e.events.Warn(ctx, event.ReasonPostStartFailed, err.Error())
			e.fail(rec, err)
			return
		}
	}

	for i := 1; i < len(pod.Spec.Containers); i++ {
		if _, _, err := volume.Materialize(volume.Request{
			Pod:              pod,
			Container:        pod.Spec.Containers[i],
			RootDir:          volRoot,
			ConfigMaps:       creds.ConfigMaps,
			Secrets:          creds.Secrets,
			ServiceToken:     creds.ServiceToken,
			AllowedHostPaths: e.cfg.AllowedHostPaths,
		}); err != nil {
			e.fail(rec, err)
			return
		}
		if err := e.sidecar.Create(ctx, pod, pod.Spec.Containers[i], volRoot); err != nil {
			e.fail(rec, err)
			return
		}
	}
	e.refreshSidecarStatus(ctx, rec)

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
	rec.ready = true
	ip := rec.pod.Status.PodIP
	rec.mu.Unlock()
	if maps := hostPortMaps(pod, ip); len(maps) > 0 {
		if err := e.hostports.Bind(Key(pod.Namespace, pod.Name), maps); err != nil {
			e.events.Warn(ctx, event.ReasonFailed, "hostPort: "+err.Error())
			e.fail(rec, err)
			return
		}
	}
	e.events.Normal(ctx, event.ReasonStarted, "macOS VM running; guest agent ready")
	go e.watch(ctx, rec)
}

func (e *Engine) fail(rec *podRecord, err error) {
	rec.mu.Lock()
	if rec.phase == corev1.PodFailed {
		rec.mu.Unlock()
		return
	}
	rec.phase = corev1.PodFailed
	rec.reason = "Failed"
	rec.message = err.Error()
	rec.failedErr = err
	rec.ready = false
	rec.mu.Unlock()
	e.teardown(context.Background(), rec, 0, false)
	e.events.Warn(context.Background(), event.ReasonFailed, err.Error())
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
	rec.agent = nil
	rec.ready = false
	consoleLn := rec.consoleLn
	rec.consoleLn = nil
	uid := ""
	ns, name := "", ""
	if pod != nil {
		uid = string(pod.UID)
		ns, name = pod.Namespace, pod.Name
	}
	rec.mu.Unlock()

	key := Key(ns, name)
	e.hostports.Release(key)
	if consoleLn != nil {
		_ = consoleLn.Close()
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
	if machine != nil {
		_ = machine.Stop(ctx, time.Duration(grace)*time.Second)
	}
	_ = e.sidecar.RemovePod(ctx, ns, name, grace)
	if warmDir != "" {
		// Adopted VM: its overlay/control tree lives under cache/warm.
		_ = os.RemoveAll(warmDir)
	}
	if uid != "" {
		e.slots.Release(uid)
	}
}

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
	rec.phase = corev1.PodSucceeded
	rec.reason = "Killing"
	rec.ready = false
	uid := string(rec.pod.UID)
	rec.mu.Unlock()

	e.events.Normal(ctx, event.ReasonKilling, "stopping macOS VM")
	e.teardown(ctx, rec, grace, true) // preStop, Shutdown, Stop — RemoveAll only after Stop returns
	e.snapshotPodCaches(rec)          // CoW the final cache state into the store
	_ = os.RemoveAll(filepath.Join(e.cfg.CacheDir, "pods", uid))

	e.mu.Lock()
	delete(e.pods, key)
	e.mu.Unlock()
	return nil
}

// Get returns a pod with status filled.
func (e *Engine) Get(namespace, name string) (*corev1.Pod, error) {
	e.mu.RLock()
	rec, ok := e.pods[Key(namespace, name)]
	e.mu.RUnlock()
	if !ok {
		return nil, errdefs.NotFound("pod not found")
	}
	return e.podFrom(rec), nil
}

// List returns all pods.
func (e *Engine) List() []*corev1.Pod {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*corev1.Pod, 0, len(e.pods))
	for _, rec := range e.pods {
		out = append(out, e.podFrom(rec))
	}
	return out
}

func (e *Engine) podFrom(rec *podRecord) *corev1.Pod {
	rec.mu.Lock()
	defer rec.mu.Unlock()
	p := rec.pod.DeepCopy()
	p.Status = e.buildStatusLocked(rec)
	return p
}

func (e *Engine) buildStatusLocked(rec *podRecord) corev1.PodStatus {
	st := rec.machine
	ip := rec.pod.Status.PodIP
	var vmState types.VMState
	if st != nil {
		vs := st.Status()
		vmState = vs.State
		if vs.IP != "" {
			ip = vs.IP
		}
	}
	started := rec.ready && rec.agentOK
	ready := rec.phase == corev1.PodRunning && rec.ready && rec.agentOK

	cs := corev1.ContainerStatus{
		Name:         rec.pod.Spec.Containers[0].Name,
		Image:        rec.pod.Spec.Containers[0].Image,
		Ready:        ready,
		Started:      &started,
		ContainerID:  "vz://" + rec.pod.Spec.Containers[0].Name,
		RestartCount: rec.restartCount,
	}
	switch rec.phase {
	case corev1.PodPending:
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: rec.reason, Message: rec.message}
	case corev1.PodFailed:
		cs.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 1, Reason: rec.reason, Message: rec.message}
	case corev1.PodSucceeded:
		cs.State.Terminated = &corev1.ContainerStateTerminated{ExitCode: 0, Reason: rec.reason, Message: rec.message}
	default:
		start := metav1.Now()
		if rec.startedAt != nil {
			start = *rec.startedAt
		}
		cs.State.Running = &corev1.ContainerStateRunning{StartedAt: start}
	}
	_ = vmState

	now := metav1.Now()
	startTime := rec.startedAt
	statuses := []corev1.ContainerStatus{cs}
	if rec.pod != nil && len(rec.pod.Spec.Containers) > 1 {
		statuses = append(statuses, sidecarContainerStatuses(rec.pod.Spec.Containers[1:], rec.sidecarStatuses)...)
	}
	return corev1.PodStatus{
		Phase:             rec.phase,
		Reason:            rec.reason,
		Message:           rec.message,
		HostIP:            e.hostIP,
		PodIP:             ip,
		StartTime:         startTime,
		ContainerStatuses: statuses,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: now},
			boolCond(corev1.PodInitialized, rec.initialized || len(rec.pod.Spec.InitContainers) == 0, now, rec),
			boolCond(corev1.ContainersReady, ready, now, rec),
			boolCond(corev1.PodReady, ready, now, rec),
		},
	}
}

func boolCond(t corev1.PodConditionType, ok bool, now metav1.Time, rec *podRecord) corev1.PodCondition {
	st := corev1.ConditionFalse
	reason := rec.reason
	msg := rec.message
	if ok {
		st = corev1.ConditionTrue
		reason = "Ready"
		msg = "guest agent ready"
	} else if reason == "" {
		reason = "NotReady"
	}
	return corev1.PodCondition{Type: t, Status: st, LastTransitionTime: now, Reason: reason, Message: msg}
}

// ExecInVM runs a command in container[0] via the guest agent.
func (e *Engine) ExecInVM(ctx context.Context, namespace, name string, cmd []string, attach api.AttachIO) error {
	e.mu.RLock()
	rec, ok := e.pods[Key(namespace, name)]
	e.mu.RUnlock()
	if !ok {
		return errdefs.NotFound("pod not found")
	}
	rec.mu.Lock()
	cli := rec.agent
	rec.mu.Unlock()
	if cli == nil {
		if !SSHFallbackEnabled(e.cfg) {
			return fmt.Errorf("guest agent not connected")
		}
		ip := ""
		rec.mu.Lock()
		if rec.pod != nil {
			ip = rec.pod.Status.PodIP
		}
		rec.mu.Unlock()
		opts := sshOptsFromConfig(e.cfg, ip)
		if opts.KnownHostsPath == "" && opts.HostKey == nil {
			return fmt.Errorf("ssh fallback requires known_hosts or a pinned host key")
		}
		stdout, stderr, code, err := guest.SSHExec(ctx, opts, cmd)
		if attach != nil {
			if attach.Stdout() != nil {
				_, _ = attach.Stdout().Write(stdout)
			}
			if attach.Stderr() != nil {
				_, _ = attach.Stderr().Write(stderr)
			}
		}
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("exit code %d", code)
		}
		return nil
	}
	req := guest.ExecReq{Argv: cmd, TTY: attach != nil && attach.TTY()}
	var stdin io.Reader
	var resize chan guest.TtyResize
	if attach != nil && attach.Stdin() != nil {
		stdin = attach.Stdin()
	}
	if attach != nil {
		if rc := attach.Resize(); rc != nil {
			resize = make(chan guest.TtyResize, 4)
			go func() {
				for sz := range rc {
					select {
					case resize <- guest.TtyResize{Cols: int(sz.Width), Rows: int(sz.Height)}:
					case <-ctx.Done():
						return
					}
				}
			}()
		}
	}
	if stdin != nil || resize != nil || (attach != nil && (attach.Stdout() != nil || attach.Stderr() != nil)) {
		var stdout, stderr io.Writer
		if attach.Stdout() != nil {
			stdout = attach.Stdout()
		}
		if attach.Stderr() != nil {
			stderr = attach.Stderr()
		}
		code, err := cli.ExecInteractive(ctx, req, stdin, resize, stdout, stderr)
		if err != nil {
			return err
		}
		if code != 0 {
			return fmt.Errorf("exit code %d", code)
		}
		return nil
	}
	_, _, code, err := cli.Exec(ctx, req)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("exit code %d", code)
	}
	return nil
}

// LogsVM streams VM logs via the guest agent.
func (e *Engine) LogsVM(ctx context.Context, namespace, name string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	e.mu.RLock()
	rec, ok := e.pods[Key(namespace, name)]
	e.mu.RUnlock()
	if !ok {
		return nil, errdefs.NotFound("pod not found")
	}
	rec.mu.Lock()
	cli := rec.agent
	machine := rec.machine
	rec.mu.Unlock()
	if cli != nil {
		if opts.Follow {
			// True follow: stream lines into the pipe as the guest appends them.
			r, w := io.Pipe()
			go func() {
				fctx, cancel := context.WithCancel(ctx)
				defer cancel()
				ferr := cli.LogsFollow(fctx, guest.LogsReq{TailLines: opts.Tail, Follow: true}, w)
				if ferr != nil && ferr != context.Canceled && ctx.Err() == nil {
					_ = w.CloseWithError(ferr)
					return
				}
				_ = w.Close()
			}()
			return r, nil
		}
		lines, err := cli.Logs(ctx, guest.LogsReq{TailLines: opts.Tail})
		if err != nil {
			return nil, err
		}
		r, w := io.Pipe()
		go func() {
			defer w.Close()
			for _, ln := range lines {
				_, _ = w.Write(ln)
				if len(ln) == 0 || ln[len(ln)-1] != '\n' {
					_, _ = w.Write([]byte("\n"))
				}
			}
		}()
		return r, nil
	}
	if machine != nil {
		return machine.Logs(), nil
	}
	return nil, fmt.Errorf("no logs available")
}

// SidecarLogs / ExecInSidecar are used by the provider for containers[1..].
func (e *Engine) SidecarLogs(ctx context.Context, ns, name, container string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	return e.sidecar.Logs(ctx, ns, name, container, opts)
}

func (e *Engine) ExecInSidecar(ctx context.Context, ns, name, container string, cmd []string, attach api.AttachIO) error {
	return e.sidecar.Exec(ctx, ns, name, container, cmd, attach)
}

// Console attaches to the pod VM's break-glass serial console. Requires a
// runtime whose machines implement runtime.Consoler (vz with
// --serial-console; the fake runtime in tests).
func (e *Engine) Console(namespace, name string) (io.ReadWriteCloser, error) {
	e.mu.RLock()
	rec, ok := e.pods[Key(namespace, name)]
	e.mu.RUnlock()
	if !ok {
		return nil, errdefs.NotFound("pod not found")
	}
	rec.mu.Lock()
	machine := rec.machine
	rec.mu.Unlock()
	if machine == nil {
		return nil, fmt.Errorf("VM not started")
	}
	cons, ok := machine.(runtime.Consoler)
	if !ok {
		return nil, fmt.Errorf("runtime %s does not support serial consoles", e.rt.Name())
	}
	return cons.Console()
}

func (e *Engine) dialFallback(ctx context.Context, token string, machine runtime.Machine) (*guest.Client, error) {
	st := machine.Status()
	ip := st.IP
	if ip == "" && st.MAC != "" {
		ip, _ = netutil.IPForMAC(st.MAC)
	}
	if ip == "" {
		return nil, fmt.Errorf("no guest IP for tcp fallback")
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ip, strconv.Itoa(types.GuestTCPPort)))
	if err != nil {
		return nil, err
	}
	return guest.Dial(ctx, conn, token, "darwin-node")
}

const agentDialTimeout = 45 * time.Second

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

// resolveImage loads a local baked directory or a previously pulled cache
// entry, then clonefile's disk+aux into the pod overlay dir.
func (e *Engine) resolveImage(ctx context.Context, ref, uid string, pullPolicy corev1.PullPolicy, creds Credentials) (disk, aux, hardware string, err error) {
	try := func(dir string) (image.LocalImage, error) {
		img, err := image.LoadDir(dir)
		if err != nil {
			return image.LocalImage{}, err
		}
		if err := img.VerifyOptional(); err != nil {
			return image.LocalImage{}, err
		}
		return img, nil
	}
	var img image.LocalImage
	if st, statErr := os.Stat(ref); statErr == nil && st.IsDir() {
		img, err = try(ref)
	} else {
		img, err = try(image.CacheDir(e.cfg.CacheDir, ref))
		if err != nil && e.puller != nil && pullPolicy != corev1.PullNever {
			img, err = e.puller.Pull(ctx, ref, creds.Pull, pullPolicy == corev1.PullAlways)
		}
	}
	if err != nil {
		return "", "", "", fmt.Errorf("load image %q: %w", ref, err)
	}
	ov, err := image.CloneOverlay(img, filepath.Join(e.cfg.CacheDir, "pods", uid, "overlay"))
	if err != nil {
		return "", "", "", err
	}
	return ov.DiskPath, ov.AuxPath, img.Config.HardwareModelData, nil
}

func decodeSSHKeyB64(s string) []byte {
	if s == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// SSHFallbackEnabled reports whether ExecInVM may use SSH when the guest agent is down.
func SSHFallbackEnabled(cfg config.Config) bool {
	return cfg.EnableSSHFallback && cfg.SSHUser != ""
}

func sshOptsFromConfig(cfg config.Config, host string) guest.SSHOpts {
	opts := guest.SSHOpts{
		User:           cfg.SSHUser,
		KeyPath:        cfg.SSHPrivateKey,
		KeyPEM:         decodeSSHKeyB64(cfg.SSHPrivateKeyB64),
		Host:           host,
		KnownHostsPath: cfg.SSHKnownHosts,
	}
	if cfg.SSHHostKey != "" {
		if pk, err := guest.ParseHostKey(cfg.SSHHostKey); err == nil {
			opts.HostKey = pk
		}
	}
	return opts
}

func (e *Engine) runInitContainers(ctx context.Context, rec *podRecord, pod *corev1.Pod, creds Credentials, volRoot string) error {
	if len(pod.Spec.InitContainers) == 0 {
		rec.mu.Lock()
		rec.initialized = true
		rec.mu.Unlock()
		return nil
	}
	for _, ic := range pod.Spec.InitContainers {
		if LooksLikeVMImage(ic.Image) {
			return fmt.Errorf("init container %q: macOS VM images cannot run as init containers", ic.Name)
		}
		if _, _, err := volume.Materialize(volume.Request{
			Pod:              pod,
			Container:        ic,
			RootDir:          volRoot,
			ConfigMaps:       creds.ConfigMaps,
			Secrets:          creds.Secrets,
			ServiceToken:     creds.ServiceToken,
			AllowedHostPaths: e.cfg.AllowedHostPaths,
		}); err != nil {
			return err
		}
		e.events.Normal(ctx, event.ReasonInitStarted, "init container "+ic.Name)
		if err := e.sidecar.Create(ctx, pod, ic, volRoot); err != nil {
			return err
		}
		if err := e.waitSidecarExit(ctx, pod.Namespace, pod.Name, ic.Name); err != nil {
			return err
		}
		e.events.Normal(ctx, event.ReasonInitExited, "init container "+ic.Name+" completed")
	}
	rec.mu.Lock()
	rec.initialized = true
	rec.mu.Unlock()
	return nil
}

func (e *Engine) waitSidecarExit(ctx context.Context, ns, name, container string) error {
	limit := e.cfg.InitTimeout
	if limit <= 0 {
		limit = 2 * time.Minute
	}
	deadline := time.Now().Add(limit)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	backoff := 50 * time.Millisecond
	const maxBackoff = time.Second
	consecutiveErr := 0
	for time.Now().Before(deadline) {
		if err := ctx.Err(); err != nil {
			return err
		}
		st, err := e.sidecar.Get(ctx, ns, name, container)
		if err != nil {
			consecutiveErr++
			if consecutiveErr >= 2 {
				return err
			}
			time.Sleep(backoff)
			continue
		}
		consecutiveErr = 0
		if st.State == "terminated" {
			if st.ExitCode != 0 {
				return fmt.Errorf("init container %s exited %d: %s", container, st.ExitCode, st.Error)
			}
			return nil
		}
		time.Sleep(backoff)
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	return fmt.Errorf("init container %s did not complete", container)
}

func (e *Engine) restartVM(ctx context.Context, rec *podRecord) error {
	rec.mu.Lock()
	machine := rec.machine
	pod := rec.pod.DeepCopy()
	places := append([]volume.Placement(nil), rec.places...)
	rec.agent = nil
	rec.mu.Unlock()
	if machine == nil {
		return fmt.Errorf("no machine to restart")
	}
	_ = machine.Stop(ctx, 5*time.Second)
	if err := machine.Start(ctx); err != nil {
		return err
	}
	cli, err := machine.DialAgent(ctx)
	if err != nil {
		return err
	}
	rec.mu.Lock()
	rec.agent = cli
	rec.agentOK = true
	rec.mu.Unlock()

	if netinfo, err := cli.NetInfo(ctx); err == nil && netinfo.PrimaryIP != "" {
		rec.mu.Lock()
		old := ""
		if rec.pod != nil {
			old = rec.pod.Status.PodIP
			rec.pod.Status.PodIP = netinfo.PrimaryIP
		}
		rec.mu.Unlock()
		if old != "" && old != netinfo.PrimaryIP {
			e.events.Normal(ctx, event.ReasonPodIPChanged, old+" -> "+netinfo.PrimaryIP)
		}
		if pod != nil {
			if maps := hostPortMaps(pod, netinfo.PrimaryIP); len(maps) > 0 {
				e.hostports.UpdateDest(Key(pod.Namespace, pod.Name), maps)
			}
		}
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
			if _, err := cli.Materialize(ctx, guest.MaterializeReq{Volumes: vp}); err != nil {
				e.events.Warn(ctx, event.ReasonGuestAgent, "rematerialize: "+err.Error())
			}
		}
	}
	e.events.Normal(ctx, event.ReasonVMRestarted, "guest agent reconnected after liveness restart")
	return nil
}

// DebugPod is a JSON-friendly engine view for the visual debugger.
type DebugPod struct {
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	UID          string `json:"uid"`
	Phase        string `json:"phase"`
	Reason       string `json:"reason"`
	Message      string `json:"message"`
	PodIP        string `json:"podIP"`
	Ready        bool   `json:"ready"`
	AgentOK      bool   `json:"agentOK"`
	RestartCount int32  `json:"restartCount"`
	VMState      string `json:"vmState"`
}

func (e *Engine) DebugSnapshot() (used, max int, pods []DebugPod) {
	used = e.slots.Used()
	max = e.slots.Max()
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, rec := range e.pods {
		rec.mu.Lock()
		dp := DebugPod{
			Namespace:    rec.pod.Namespace,
			Name:         rec.pod.Name,
			UID:          string(rec.pod.UID),
			Phase:        string(rec.phase),
			Reason:       rec.reason,
			Message:      rec.message,
			PodIP:        rec.pod.Status.PodIP,
			Ready:        rec.ready,
			AgentOK:      rec.agentOK,
			RestartCount: rec.restartCount,
		}
		if rec.machine != nil {
			dp.VMState = string(rec.machine.Status().State)
			if rec.machine.Status().IP != "" {
				dp.PodIP = rec.machine.Status().IP
			}
		}
		rec.mu.Unlock()
		pods = append(pods, dp)
	}
	return used, max, pods
}

func (e *Engine) PodMetrics(ctx context.Context, ns, name string) (guest.MetricsRes, bool) {
	e.mu.RLock()
	rec, ok := e.pods[Key(ns, name)]
	e.mu.RUnlock()
	if !ok {
		return guest.MetricsRes{}, false
	}
	rec.mu.Lock()
	cli := rec.agent
	rec.mu.Unlock()
	if cli == nil {
		return guest.MetricsRes{}, false
	}
	m, err := cli.Metrics(ctx)
	if err != nil {
		return guest.MetricsRes{}, false
	}
	return m, true
}
