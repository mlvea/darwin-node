// Package engine is the pod lifecycle state machine.
package engine

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mlvea/darwin-node/pkg/capacity"
	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/hostport"
	"github.com/mlvea/darwin-node/pkg/image"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/volume"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/flowcontrol"
)

// Key is namespace/name.
func Key(ns, name string) string { return ns + "/" + name }

// Credentials are volume-related secrets resolved by the provider.
type Credentials struct {
	ConfigMaps   map[string]*corev1.ConfigMap
	Secrets      map[string]*corev1.Secret
	ServiceToken string
	Tokens       []volume.ProjectedToken
	Pull         image.RegistryCreds
}

// TokenMinter issues a fresh set of projected service-account tokens.
// A nil minter leaves tokens as they were at pod start.
type TokenMinter func(ctx context.Context, pod *corev1.Pod) ([]volume.ProjectedToken, error)

func (c Credentials) volumeReq(pod *corev1.Pod, container corev1.Container, root string, allowed []string) volume.Request {
	return volume.Request{
		Pod:              pod,
		Container:        container,
		RootDir:          root,
		ConfigMaps:       c.ConfigMaps,
		Secrets:          c.Secrets,
		ServiceToken:     c.ServiceToken,
		Tokens:           c.Tokens,
		AllowedHostPaths: allowed,
	}
}

// Engine owns in-memory pod state and drives the runtime.
type Engine struct {
	cfg            config.Config
	slots          *capacity.Slots
	rt             runtime.Runtime
	sidecar        sidecar.Runtime
	events         event.Recorder
	hostIP         string
	puller         image.Puller
	hostports      *hostport.Manager
	minter         TokenMinter
	sidecarBackOff *flowcontrol.Backoff

	warm       warmPool
	warmCancel context.CancelFunc
	warmCtx    context.Context
	warmBootWG sync.WaitGroup
	lastImage  atomic.Value // string: most recently booted image ref
	draining   atomic.Bool

	closeOnce    sync.Once
	closeErr     error
	closeTimeout time.Duration // tests shorten the deadline; zero uses defaultCloseTimeout
	clock        probeClock    // tests move this; nil is the wall clock
	reapTimeout  time.Duration // tests shorten the post-deadline reap; zero uses defaultReapTimeout

	// Abandoned boots are VMs whose Create or Start was still running when
	// Close returned. flights counts those calls. The reap polls it instead
	// of waiting on a goroutine that would outlive the timeout.
	abandonedMu sync.Mutex
	inflight    map[string]abandonedBoot
	abandoned   []abandonedBoot
	settled     map[string]struct{}
	flights     int

	// cacheSnapMu serializes snapshot and restore of one cache store.
	// Two pods in a namespace can name the same store, and Close deletes
	// those pods together.
	cacheSnapMu sync.Mutex

	mu   sync.RWMutex
	pods map[string]*podRecord

	// interrupted lists pod UIDs whose disks were on disk when this process
	// started. restartCredit is the restartCount to publish when one of
	// those pods is booted again.
	interrupted   map[string]struct{}
	restartCredit map[string]int32

	// adopted is a supervisor VM reattached at startup and not yet handed
	// to Create. Close stops whatever is still here.
	adopted map[string]runtime.Machine
}

// defaultCloseTimeout bounds Close when Create, Start, Stop, or RemovePod
// does not return. The slot stays held until that call finishes.
const defaultCloseTimeout = 30 * time.Second

// condTransition is the last time a pod condition's status changed.
// LastTransitionTime stays at `at` until `status` changes.
type condTransition struct {
	status corev1.ConditionStatus
	at     metav1.Time
}

type podRecord struct {
	mu sync.Mutex

	pod     *corev1.Pod
	machine runtime.Machine
	agent   *guest.Client
	places  []volume.Placement
	token   string
	phase   corev1.PodPhase
	// failing is set before teardown so a second fail does not stop the
	// VM twice, and Failed is published only after the slot is released.
	failing      bool
	reason       string
	message      string
	startedAt    *metav1.Time
	agentOK      bool
	ready        bool
	startupDone  bool      // startup probe passed, or there is no startup probe
	startupSince time.Time // anchor for the startup initial delay; zero uses startedAt
	// guestExit is the last Exit notify from this container generation.
	// Nil means the guest sent none. A liveness restart clears it.
	guestExit       *int32
	guestExitReason string
	// killedCode is set when Delete stops a pod whose workload has not
	// exited. 137 is SIGKILL (grace 0) and 143 is SIGTERM (grace > 0).
	// It wins over a later guest frame so the published code stays the kill.
	killedCode      *int32
	agentGen        uint64
	failedErr       error
	cancel          context.CancelFunc
	restartCount    int32
	transitions     map[corev1.PodConditionType]condTransition
	initialized     bool
	sidecarStatuses []sidecar.Status
	sidecarRestarts map[string]int32
	sidecarLastExit map[string]*corev1.ContainerStateTerminated
	sidecarStart    map[string]*sidecarStartState
	sidecarWait     map[string]sidecarWait
	sidecarExitAt   map[string]sidecarExitMark
	initStatuses    []corev1.ContainerStatus
	warmDir         string // set when the VM was adopted from the warm pool
	warmDisk        string // cache/pods/<warm-slot> overlay; not the pod uid dir
	// liveBackoff is the next liveness restart delay. liveBackoffAt is when
	// the last liveness restart happened. Ten minutes of healthy running
	// resets the delay to 10s.
	liveBackoff     time.Duration
	liveBackoffAt   time.Time
	consoleLn       net.Listener // per-VM unix socket for break-glass console
	consoleSock     string
	consoleHub      *consoleHub
	startDone       sync.WaitGroup // closed when the async start goroutine returns
	watchDone       sync.WaitGroup // closed when the probe and restart watch returns
	emptyDirs       []volume.EmptyDirLimit
	volRoot         string
	tokens          []volume.ProjectedToken
	refreshAt       time.Time
	tokensNeedCopy  bool
	hostPortBind    string // address reserved at admission; empty means every interface
	hostPortBindSet bool
	hostPortOn      bool // proxy is bound; later IP updates retarget it
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
	configurePuller(puller, cfg)
	e := &Engine{
		cfg:            cfg,
		slots:          slots,
		rt:             rt,
		sidecar:        sc,
		events:         rec,
		hostIP:         hostIP,
		puller:         puller,
		hostports:      hostport.New(),
		sidecarBackOff: flowcontrol.NewBackOff(sidecarRestartBackoff, sidecarRestartBackoffMax),
		pods:           map[string]*podRecord{},
	}
	_ = e.Recover()
	if cfg.WarmSlots > 0 {
		e.startWarmPool()
	}
	return e
}

// configurePuller copies the image-pull budget onto the cache. Zero values
// keep the image package defaults (256 GiB declared and compressed, 10 GiB
// free, 30m deadline, 2m read stall).
func configurePuller(m *image.Manager, cfg config.Config) {
	m.InsecureRegistries = cfg.InsecureRegistries
	m.PullTimeout = cfg.ImagePullTimeout
	m.ReadStall = cfg.ImageReadStall
	m.MaxImageBytes = cfg.MaxImageBytes
	m.MaxCompressedBytes = cfg.MaxImageCompressedBytes
	m.DiskHeadroom = cfg.ImageDiskHeadroom
}

func (e *Engine) Slots() *capacity.Slots { return e.slots }

// SetTokenMinter installs the projected-token refresher used by the watch loop.
func (e *Engine) SetTokenMinter(m TokenMinter) {
	e.mu.Lock()
	e.minter = m
	e.mu.Unlock()
}
