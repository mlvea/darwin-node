package simvz

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"

	"github.com/mlvea/darwin-node/pkg/guest"
)

// errStartFailed is the injected start() failure. The text contains
// start() so a log line can be matched without importing this sentinel.
var errStartFailed = fmt.Errorf("simvz: start() failed")

func listenStream() (string, net.Listener, error) {
	path, err := shortSock("simvz")
	if err != nil {
		return "", nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		_ = os.Remove(path)
		return "", nil, err
	}
	return path, ln, nil
}

func shortSock(prefix string) (string, error) {
	f, err := os.CreateTemp("/tmp", prefix+"-*.sock")
	if err != nil {
		return "", err
	}
	name := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", err
	}
	if err := os.Remove(name); err != nil {
		return "", err
	}
	return name, nil
}

func (m *machine) serveInProcess(ctx context.Context) error {
	path, ln, err := listenStream()
	if err != nil {
		return err
	}
	h := m.inProcessHandler()
	m.mu.Lock()
	m.sock = path
	m.ln = ln
	m.mu.Unlock()
	m.wg.Add(1)
	go m.acceptLoop(ctx, ln, h)
	return nil
}

func (m *machine) inProcessHandler() guest.Handler {
	h := guest.Handler{
		Token:                m.token,
		AgentVersion:         "simvz",
		IdleTimeout:          m.rt.idle,
		AllowInsecureNoToken: m.token == "",
		NetInfoFn: func() (guest.NetInfoRes, error) {
			return guest.NetInfoRes{
				PrimaryIP: m.ip,
				IPs:       []string{m.ip},
				MAC:       m.mac,
				IFName:    "simvz0",
			}, nil
		},
	}
	h.Init()
	return h
}

func (m *machine) acceptLoop(ctx context.Context, ln net.Listener, h guest.Handler) {
	defer m.wg.Done()
	var serves sync.WaitGroup
	defer serves.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		serves.Add(1)
		go func(conn net.Conn) {
			defer serves.Done()
			_ = guest.Serve(ctx, conn, h)
		}(c)
	}
}

// DialAgent connects to the guest's unix socket and performs the host
// handshake. The VM must be Running. A crashed guest fails the dial and
// leaves the machine Running.
func (m *machine) DialAgent(ctx context.Context) (*guest.Client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.state != vzRunning {
		st := m.state.String()
		m.mu.Unlock()
		return nil, &StateError{Op: "dial", State: st}
	}
	path := m.sock
	token := m.token
	life := m.life
	m.mu.Unlock()
	if path == "" {
		return nil, fmt.Errorf("simvz: guest agent is not listening")
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("simvz: vsock dial: %w", err)
	}
	wrapped := m.wrap(life, ctx, conn)
	cli, err := guest.Dial(ctx, wrapped, token, "simvz")
	if err != nil {
		_ = wrapped.Close()
		return nil, err
	}
	return cli, nil
}

func (m *machine) wrap(life, dial context.Context, conn net.Conn) net.Conn {
	if m.faults.Stall <= 0 && m.faults.VsockEOFAfter <= 0 {
		return conn
	}
	return &faultConn{
		Conn:     conn,
		clock:    m.clock,
		ctx:      life,
		dial:     dial,
		stall:    m.faults.Stall,
		eofAfter: m.faults.VsockEOFAfter,
	}
}

func (m *machine) crashGuest() {
	m.mu.Lock()
	ln := m.ln
	m.ln = nil
	proc := m.proc
	m.proc = nil
	m.agentOK = false
	m.mu.Unlock()
	m.cancelLife()
	if ln != nil {
		_ = ln.Close()
	}
	if proc != nil {
		proc.kill()
	}
}

func (m *machine) armCrash(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.clock.Sleep(ctx, m.faults.CrashAfter); err != nil {
			return
		}
		m.crashGuest()
	}()
}
