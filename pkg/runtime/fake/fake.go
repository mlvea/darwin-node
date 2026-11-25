// Package fake is an in-process VM runtime for tests and --runtime=fake.
package fake

import (
	"context"
	"fmt"
	"io"
	"sync"
	"sync/atomic"

	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/types"
)

// Runtime implements runtime.Runtime without Virtualization.framework.
type Runtime struct {
	Token         string
	IP            string
	IPs           []string // if set, Start() rotates through these as PrimaryIP
	ExecFn        func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	ProbeFn       func(ctx context.Context, req guest.ProbeReq) guest.ProbeRes
	MetricsFn     func() (guest.MetricsRes, error)
	MaterializeFn func(req guest.MaterializeReq) guest.MaterializeRes
	// WorkloadFn handles the VM container command. Nil accepts the RPC
	// and does not start a process. The engine tests that apply a pod
	// command record the request here.
	WorkloadFn func(ctx context.Context, req guest.WorkloadReq) error
	// FailDialAfter makes DialAgent fail once the machine has already
	// dialed this many times. Zero never fails. Start does not count.
	FailDialAfter int

	// ExitCode is copied onto every machine status when non-nil. The vz
	// runtime leaves this unset; PodFailed then keeps exit code 1.
	ExitCode *int32

	starts   int32
	running  int32
	matCalls int32

	last atomic.Pointer[machine]

	mu    sync.Mutex
	byUID map[string]*machine
}

func (r *Runtime) MaterializeCalls() int { return int(atomic.LoadInt32(&r.matCalls)) }

func (r *Runtime) Create(_ context.Context, spec types.VMSpec) (runtime.Machine, error) {
	token := spec.AgentToken
	if token == "" {
		token = r.Token
	}
	ip := r.IP
	if ip == "" {
		ip = "192.168.64.2"
	}
	m := &machine{
		rt:    r,
		id:    spec.ID,
		token: token,
		ip:    ip,
		mac:   spec.MAC,
		logs:  guest.NewLogBuffer(64),
	}
	r.last.Store(m)
	r.mu.Lock()
	if r.byUID == nil {
		r.byUID = map[string]*machine{}
	}
	r.byUID[spec.ID.UID] = m
	r.mu.Unlock()
	return m, nil
}

// ReportExitUID sends an Exit notify from the machine created for uid.
// The host records it on the guest read loop. StopUID must wait until that
// record exists, or a stop with no code is a failure and the cache is kept.
func (r *Runtime) ReportExitUID(uid string, rep guest.ExitReport) error {
	m := r.machineByUID(uid)
	if m == nil {
		return fmt.Errorf("no fake machine %s", uid)
	}
	m.ReportExit(rep)
	return nil
}

// StopUID stops the machine created for uid.
func (r *Runtime) StopUID(ctx context.Context, uid string) error {
	m := r.machineByUID(uid)
	if m == nil {
		return fmt.Errorf("no fake machine %s", uid)
	}
	return m.Stop(ctx, 0)
}

func (r *Runtime) machineByUID(uid string) *machine {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.byUID == nil {
		return nil
	}
	return r.byUID[uid]
}

// ConsoleGuestEnd returns the simulated guest side of the most recently
// created machine's serial console pipe. Test support.
func (r *Runtime) ConsoleGuestEnd() io.ReadWriteCloser {
	if m := r.last.Load(); m != nil {
		return m.ConsoleGuestEnd()
	}
	return nil
}

func (r *Runtime) Starts() int { return int(atomic.LoadInt32(&r.starts)) }

// Running is how many machines are in Starting or Running. Tests use it to
// catch VMs that outlive the slot table.
func (r *Runtime) Running() int { return int(atomic.LoadInt32(&r.running)) }

func New() *Runtime {
	return &Runtime{Token: "fake-token", IP: "192.168.64.2"}
}

func (r *Runtime) Name() types.RuntimeName { return types.RuntimeFake }

// AgentToken is the handshake token of the most recently created machine.
func (r *Runtime) AgentToken() string {
	if m := r.last.Load(); m != nil {
		return m.AgentToken()
	}
	return ""
}

// ReportExit sends an Exit notify from the guest of the most recently
// created machine.
func (r *Runtime) ReportExit(rep guest.ExitReport) {
	if m := r.last.Load(); m != nil {
		m.ReportExit(rep)
	}
}

// DropAgent closes the guest side of the most recently created machine.
func (r *Runtime) DropAgent() {
	if m := r.last.Load(); m != nil {
		m.DropAgent()
	}
}

// AgentDials is how many times the latest machine's DialAgent was called.
func (r *Runtime) AgentDials() int {
	if m := r.last.Load(); m != nil {
		return int(m.dials.Load())
	}
	return 0
}
