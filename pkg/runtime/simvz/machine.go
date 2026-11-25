package simvz

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"math/rand/v2"

	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/types"
)

// Name is the runtime name reported to the engine. It is not RuntimeVZ, so
// the engine does not try to pull a disk image before Create.
const Name types.RuntimeName = "simvz"

// Runtime is the simulator. Machines share one VM quota and one FailStart
// budget.
type Runtime struct {
	clock      Clock
	faults     Faults
	guestBin   string
	workDir    string
	transition time.Duration
	max        int
	idle       time.Duration

	mu       sync.Mutex
	live     int
	failLeft int
	seq      atomic.Uint64

	order     string
	orderSeed int64
	rng       *rand.Rand
	rngMu     sync.Mutex
	gateMu    sync.Mutex
	gates     map[string]*gate
}

// New builds a simulator. A nil clock is the wall clock. See Options.
func New(opts Options) *Runtime {
	clock := opts.Clock
	if clock == nil {
		clock = wallClock{}
	}
	faults := opts.resolvedFaults()
	rt := &Runtime{
		clock:      clock,
		faults:     faults,
		guestBin:   opts.GuestBinary,
		workDir:    opts.Dir,
		transition: opts.Transition,
		max:        opts.maxRunning(),
		idle:       idleWall(clock),
		failLeft:   faults.FailStart,
	}
	rt.initOrder(opts)
	return rt
}

// idleWall is the guest idle timeout on the real clock. The protocol
// default is 30s. A scaled clock shrinks it so the watchdog and the stall
// stay in the same ratio.
func idleWall(clock Clock) time.Duration {
	if clock == nil {
		return guest.DefaultIdleTimeout
	}
	d := clock.Wall(guest.DefaultIdleTimeout)
	if d <= 0 {
		return guest.DefaultIdleTimeout
	}
	return d
}

func (r *Runtime) Name() types.RuntimeName { return Name }

// Running is how many VMs currently hold a slot (anything not Stopped).
func (r *Runtime) Running() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.live
}

// Create records a stopped machine. It does not take a VM slot; Start does.
func (r *Runtime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r == nil {
		return nil, fmt.Errorf("simvz: nil runtime")
	}
	return newMachine(r, spec), nil
}

func (r *Runtime) reserve() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live >= r.max {
		return limitError{}
	}
	r.live++
	return nil
}

func (r *Runtime) release() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.live > 0 {
		r.live--
	}
}

func (r *Runtime) consumeStartFailure() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failLeft <= 0 {
		return false
	}
	r.failLeft--
	return true
}

type machine struct {
	rt     *Runtime
	clock  Clock
	faults Faults
	spec   types.VMSpec
	id     types.MachineID
	token  string
	ip     string
	mac    string

	mu         sync.Mutex
	state      vzState
	held       bool
	started    *time.Time
	finished   *time.Time
	agentOK    bool
	bootErr    error
	dir        string
	disk       string
	sock       string
	hostNIC    *os.File
	guestNIC   *os.File
	ln         net.Listener
	proc       *guestProc
	life       context.Context
	lifeCancel context.CancelFunc

	wg           sync.WaitGroup
	events       []traceEvent
	agentReady   bool
	flow         *flowGroup
	dgramLeft    int
	snapshotPath string
	q            vmQueue
}

func newMachine(r *Runtime, spec types.VMSpec) *machine {
	n := r.seq.Add(1)
	ip := fmt.Sprintf("192.168.64.%d", int(n)+1)
	mac := spec.MAC
	if mac == "" {
		mac = fmt.Sprintf("02:00:00:00:%02x:%02x", byte(n>>8), byte(n))
	}
	return &machine{
		rt:        r,
		clock:     r.clock,
		faults:    r.faults,
		spec:      spec,
		id:        spec.ID,
		token:     spec.AgentToken,
		ip:        ip,
		mac:       mac,
		state:     vzStopped,
		flow:      newFlowGroup(),
		dgramLeft: r.faults.DatagramENOBUFS,
	}
}

func (m *machine) ID() types.MachineID { return m.id }

// State is the framework state name (Stopped, Starting, Running, Pausing,
// Paused, Saving, Stopping). Status maps that onto types.VMState.
func (m *machine) State() string {
	if m == nil {
		return vzStopped.String()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.String()
}

// DiskPath is the cloned disk image while the VM has been booted and not
// yet cleaned up. Empty before the clone and after Stop.
func (m *machine) DiskPath() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.disk
}

// NetworkFiles returns the datagram socketpair while the VM holds it.
// Both are nil once Stop or a failed Start has closed them. The caller
// must not close the files.
func (m *machine) NetworkFiles() (host, guest *os.File) {
	if m == nil {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hostNIC, m.guestNIC
}

func (m *machine) Status() types.VMStatus {
	if m == nil {
		return types.VMStatus{State: types.VMPending}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	st := types.VMStatus{
		State:      m.state.exposed(),
		IP:         m.ip,
		IPs:        []string{m.ip},
		MAC:        m.mac,
		StartedAt:  m.started,
		FinishedAt: m.finished,
		AgentOK:    m.agentOK && (m.state == vzRunning || m.state == vzPausing || m.state == vzPaused),
		Transport:  types.TransportVsock,
		Message:    m.state.String(),
		Err:        m.bootErr,
	}
	return st
}

func (m *machine) Logs() io.ReadCloser {
	return io.NopCloser(stringsReader("simvz: guest logs are served by the agent\n"))
}

func (m *machine) cas(from, to vzState, op string) error {
	m.mu.Lock()
	if m.state != from {
		st := m.state.String()
		m.mu.Unlock()
		return &StateError{Op: op, State: st}
	}
	release := m.commitLocked(to)
	m.mu.Unlock()
	m.releaseSlot(release)
	return nil
}

func (m *machine) pauseTransition(ctx context.Context) error {
	if m.rt == nil || m.rt.transition <= 0 {
		return nil
	}
	return m.clock.Sleep(ctx, m.rt.transition)
}

func waitWall(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	if ctx == nil {
		<-t.C
		return
	}
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

func (m *machine) failStart(err error) {
	m.cleanup()
	m.mu.Lock()
	m.agentOK = false
	m.bootErr = err
	m.mu.Unlock()
	m.become(vzStopped)
	m.recordCompletion(err)
}

// shutdown stops the actor and any vsock pumps. Stop and failStart already
// do this; call it again for a VM left in Error, which still has an actor.
func (m *machine) shutdown() {
	if m == nil {
		return
	}
	m.dropFlows()
	m.q.stop()
}

func stringsReader(s string) io.Reader { return &stringReader{s: s} }

type stringReader struct {
	s string
	i int
}

func (r *stringReader) Read(p []byte) (int, error) {
	if r.i >= len(r.s) {
		return 0, io.EOF
	}
	n := copy(p, r.s[r.i:])
	r.i += n
	return n, nil
}
