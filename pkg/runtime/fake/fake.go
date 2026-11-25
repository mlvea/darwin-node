// Package fake is an in-process VM runtime for tests and --runtime=fake.
package fake

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

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
	return m, nil
}

// ConsoleGuestEnd returns the simulated guest side of the most recently
// created machine's serial console pipe. Test support.
func (r *Runtime) ConsoleGuestEnd() io.ReadWriteCloser {
	if m := r.last.Load(); m != nil {
		return m.ConsoleGuestEnd()
	}
	return nil
}

// defaultMetrics is the fake guest agent's known usage. Tests override MetricsFn.
func defaultMetrics() (guest.MetricsRes, error) {
	return guest.MetricsRes{CPUNanoCores: 1_000_000, MemoryWorkingSet: 64 << 20}, nil
}

func (r *Runtime) Starts() int { return int(atomic.LoadInt32(&r.starts)) }

// Running is how many machines are in Starting or Running. Tests use it to
// catch VMs that outlive the slot table.
func (r *Runtime) Running() int { return int(atomic.LoadInt32(&r.running)) }

func New() *Runtime {
	return &Runtime{Token: "fake-token", IP: "192.168.64.2"}
}

func (r *Runtime) Name() types.RuntimeName { return types.RuntimeFake }

type machine struct {
	rt    *Runtime
	id    types.MachineID
	token string
	ip    string
	mac   string
	logs  *guest.LogBuffer

	consoleOnce  sync.Once
	consoleHost  io.ReadWriteCloser
	consoleGuest io.ReadWriteCloser

	mu        sync.Mutex
	state     types.VMState
	started   *time.Time
	finished  *time.Time
	agentOK   bool
	counted   bool
	srvCancel context.CancelFunc
	sctx      context.Context
	handler   guest.Handler
	client    *guest.Client
	peer      net.Conn
	agentConn net.Conn
	servers   []net.Conn
	serves    *serveSet
	dials     atomic.Int32
}

// serveSet is one generation of guest Serves. Stop waits on the current
// set and then drops it, so a Start that races the wait Adds to a new set
// instead of a WaitGroup that is already waiting.
type serveSet struct {
	wg sync.WaitGroup
}

// Console exposes a break-glass serial stream. The guest side is available
// to tests via ConsoleGuestEnd; in production the guest writes to its own end.
func (m *machine) Console() (io.ReadWriteCloser, error) {
	m.consoleOnce.Do(func() {
		m.consoleHost, m.consoleGuest = net.Pipe()
	})
	return m.consoleHost, nil
}

// ConsoleGuestEnd returns the simulated guest side of the console pipe.
func (m *machine) ConsoleGuestEnd() io.ReadWriteCloser {
	_, _ = m.Console()
	return m.consoleGuest
}

func (m *machine) ID() types.MachineID { return m.id }

// AgentToken is the handshake token the in-process guest will accept.
func (m *machine) AgentToken() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.token
}

// SetAgentToken rotates the handshake token. The fake guest does not watch
// the control-share file; the vz guest does. Tests and warm adoption call
// this so the next dial matches the token written for the pod.
func (m *machine) SetAgentToken(token string) {
	m.mu.Lock()
	m.token = token
	h := m.handler
	m.mu.Unlock()
	h.SetToken(token)
	m.mu.Lock()
	m.handler = h
	m.mu.Unlock()
}

// AgentToken is the handshake token of the most recently created machine.
func (r *Runtime) AgentToken() string {
	if m := r.last.Load(); m != nil {
		return m.AgentToken()
	}
	return ""
}

// ReportExit asks the in-process guest to send an Exit notify. Tests use it
// for the same frame a real guest writes on vsock before it halts.
func (m *machine) ReportExit(rep guest.ExitReport) {
	m.mu.Lock()
	h := m.handler
	m.mu.Unlock()
	h.ReportExit(rep)
}

// ReportExit sends an Exit notify from the guest of the most recently
// created machine.
func (r *Runtime) ReportExit(rep guest.ExitReport) {
	if m := r.last.Load(); m != nil {
		m.ReportExit(rep)
	}
}

func (m *machine) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == types.VMRunning {
		return nil
	}
	m.state = types.VMStarting
	if m.rt != nil {
		n := atomic.AddInt32(&m.rt.starts, 1)
		if !m.counted {
			atomic.AddInt32(&m.rt.running, 1)
			m.counted = true
		}
		if len(m.rt.IPs) > 0 {
			m.ip = m.rt.IPs[int(n-1)%len(m.rt.IPs)]
		}
	}
	a, b := net.Pipe()
	sctx, cancel := context.WithCancel(context.Background())
	m.srvCancel = cancel
	var execFn func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	if m.rt != nil {
		execFn = m.rt.ExecFn
	}
	if execFn == nil {
		execFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			_, _ = stdout.Write([]byte("fake-exec\n"))
			return 0, nil
		}
	}
	metricsFn := defaultMetrics
	if m.rt != nil && m.rt.MetricsFn != nil {
		metricsFn = m.rt.MetricsFn
	}
	h := guest.Handler{
		Token:        m.token,
		AgentVersion: "fake",
		LogBuffer:    m.logs,
		NetInfoFn: func() (guest.NetInfoRes, error) {
			return guest.NetInfoRes{PrimaryIP: m.ip, IPs: []string{m.ip}, MAC: m.mac, IFName: "en0"}, nil
		},
		ExecFn:    execFn,
		MetricsFn: metricsFn,
		WorkloadFn: func(ctx context.Context, req guest.WorkloadReq) error {
			if m.rt != nil && m.rt.WorkloadFn != nil {
				return m.rt.WorkloadFn(ctx, req)
			}
			return nil
		},
		MaterializeFn: func(req guest.MaterializeReq) guest.MaterializeRes {
			if m.rt != nil {
				atomic.AddInt32(&m.rt.matCalls, 1)
				if m.rt.MaterializeFn != nil {
					return m.rt.MaterializeFn(req)
				}
			}
			var placed []string
			for _, v := range req.Volumes {
				placed = append(placed, v.GuestPath)
			}
			return guest.MaterializeRes{OK: true, Placed: placed}
		},
	}
	if m.rt != nil && m.rt.ProbeFn != nil {
		pf := m.rt.ProbeFn
		h.ProbeHTTPFn = func(ctx context.Context, req guest.ProbeReq) guest.ProbeRes {
			return pf(ctx, req)
		}
	}
	h.Init()
	m.sctx = sctx
	m.handler = h
	m.trackServeLocked(sctx, a, h)
	cli, err := guest.Dial(ctx, b, m.token, "fake-host")
	if err != nil {
		cancel()
		m.state = types.VMFailed
		return err
	}
	now := time.Now()
	m.started = &now
	m.state = types.VMRunning
	m.agentOK = true
	m.client = cli
	m.peer = b
	_, _ = m.logs.Write([]byte("fake vm started\n"))
	return nil
}

func (m *machine) Stop(_ context.Context, _ time.Duration) error {
	m.mu.Lock()
	if m.counted && m.rt != nil {
		atomic.AddInt32(&m.rt.running, -1)
		m.counted = false
	}
	// A machine that never reached Start has no guest. One that did must
	// still join Serve. Cancelling the context only schedules idleWatchdog,
	// and returning before that goroutine leaves select is what the macOS
	// race run reported. Handler.Close wakes every Serve that shares this
	// handler, including a dial whose client the host still holds.
	started := m.state != types.VMStopped && m.state != "" && m.state != types.VMPending
	cancel := m.srvCancel
	m.srvCancel = nil
	client := m.client
	m.client = nil
	servers := m.servers
	m.servers = nil
	m.agentConn = nil
	m.peer = nil
	handler := m.handler
	if started && m.finished == nil {
		now := time.Now()
		m.finished = &now
	}
	set := m.serves
	m.serves = nil
	m.state = types.VMStopped
	m.agentOK = false
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	handler.Close()
	for _, c := range servers {
		_ = c.Close()
	}
	if client != nil {
		_ = client.Close()
	}
	if set != nil {
		set.wg.Wait()
	}
	return nil
}

// trackServeLocked starts a guest Serve and remembers its conn. The caller
// holds m.mu. Stop waits on the set it observed and leaves a nil set, so
// the next Start does not Add to a WaitGroup that is already in Wait.
func (m *machine) trackServeLocked(ctx context.Context, server net.Conn, h guest.Handler) {
	if m.serves == nil {
		m.serves = &serveSet{}
	}
	set := m.serves
	set.wg.Add(1)
	m.servers = append(m.servers, server)
	go func() {
		defer set.wg.Done()
		_ = guest.Serve(ctx, server, h)
	}()
}

func (m *machine) Status() types.VMStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := types.VMStatus{
		State:      m.state,
		IP:         m.ip,
		IPs:        []string{m.ip},
		MAC:        m.mac,
		StartedAt:  m.started,
		FinishedAt: m.finished,
		AgentOK:    m.agentOK,
		Transport:  types.TransportLoop,
	}
	if m.rt != nil {
		st.ExitCode = m.rt.ExitCode
	}
	if m.state == "" {
		st.State = types.VMPending
	}
	return st
}

func (m *machine) DialAgent(ctx context.Context) (*guest.Client, error) {
	n := m.dials.Add(1)
	m.mu.Lock()
	if m.state != types.VMRunning || m.sctx == nil {
		m.mu.Unlock()
		return nil, context.Canceled
	}
	failAfter := 0
	if m.rt != nil {
		failAfter = m.rt.FailDialAfter
	}
	if failAfter > 0 && int(n) > failAfter {
		m.mu.Unlock()
		return nil, fmt.Errorf("agent down")
	}
	h := m.handler
	sctx := m.sctx
	token := m.token
	// A new pipe per dial. The guest agent serves one connection and then
	// dials again; closing the previous client must not retire the VM.
	// The pipe is created under the lock so Stop cannot begin its wait
	// until this Serve is on the current set.
	a, b := net.Pipe()
	m.agentConn = a
	m.trackServeLocked(sctx, a, h)
	m.mu.Unlock()
	return guest.Dial(ctx, b, token, "fake-host")
}

// DropAgent closes the guest side of the latest DialAgent pipe. The VM
// stays running. The host session ends and the engine redials.
func (m *machine) DropAgent() {
	m.mu.Lock()
	c := m.agentConn
	m.mu.Unlock()
	if c != nil {
		_ = c.Close()
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

func (m *machine) Logs() io.ReadCloser {
	r, w := io.Pipe()
	go func() {
		defer w.Close()
		for _, ln := range m.logs.Tail(0) {
			_, _ = w.Write(ln)
		}
	}()
	return r
}
