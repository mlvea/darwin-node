// Package fake is an in-process VM runtime for tests and --runtime=fake.
package fake

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/types"
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

	starts   int32
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
	srvCancel context.CancelFunc
	client    *guest.Client
	peer      net.Conn
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

func (m *machine) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == types.VMRunning {
		return nil
	}
	m.state = types.VMStarting
	if m.rt != nil {
		n := atomic.AddInt32(&m.rt.starts, 1)
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
	go func() { _ = guest.Serve(sctx, a, h) }()
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
	defer m.mu.Unlock()
	m.state = types.VMStopping
	if m.client != nil {
		_ = m.client.Close()
		m.client = nil
	}
	if m.srvCancel != nil {
		m.srvCancel()
	}
	now := time.Now()
	m.finished = &now
	m.state = types.VMStopped
	m.agentOK = false
	return nil
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
	if m.state == "" {
		st.State = types.VMPending
	}
	return st
}

func (m *machine) DialAgent(ctx context.Context) (*guest.Client, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.client == nil {
		return nil, context.Canceled
	}
	return m.client, nil
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
