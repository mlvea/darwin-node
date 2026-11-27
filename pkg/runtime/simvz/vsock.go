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
	g := m.gen
	if m.ln != nil || m.agentReady {
		m.mu.Unlock()
		_ = ln.Close()
		_ = os.Remove(path)
		return nil
	}
	if g == nil {
		m.mu.Unlock()
		_ = ln.Close()
		_ = os.Remove(path)
		return fmt.Errorf("simvz: serve: machine has no boot generation")
	}
	m.sock = path
	m.ln = ln
	m.agentReady = true
	m.bindGenLocked()
	g.wg.Add(1)
	m.mu.Unlock()
	go m.acceptLoop(ctx, ln, h, g)
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

func (m *machine) acceptLoop(ctx context.Context, ln net.Listener, h guest.Handler, g *generation) {
	defer g.wg.Done()
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
		idle:     guest.DefaultIdleTimeout,
		eofAfter: m.faults.VsockEOFAfter,
	}
}

func (m *machine) crashGuest() {
	m.crashGen(nil)
}

// crashGen kills only. nil means the generation that is current when the
// lock is taken. A delayed crash whose generation Stop has already
// replaced does nothing: closing the new boot's listener would undo a
// Start that overlapped that Stop.
func (m *machine) crashGen(only *generation) {
	m.mu.Lock()
	if only != nil && m.gen != only {
		m.mu.Unlock()
		return
	}
	ln := m.ln
	proc := m.proc
	m.ln = nil
	m.proc = nil
	m.agentOK = false
	m.agentReady = false
	cancel := m.lifeCancel
	m.lifeCancel = nil
	oldFlow := m.flow
	ng := newFlowGroup()
	m.flow = ng
	if m.gen != nil {
		m.gen.ln = nil
		m.gen.proc = nil
		m.gen.cancel = nil
		m.gen.flow = ng
	}
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if oldFlow != nil {
		oldFlow.close()
	}
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
	m.mu.Lock()
	g := m.gen
	if g != nil {
		g.wg.Add(1)
	}
	m.mu.Unlock()
	if g == nil {
		return
	}
	go func() {
		defer g.wg.Done()
		if err := m.clock.Sleep(ctx, m.faults.CrashAfter); err != nil {
			return
		}
		m.crashGen(g)
	}()
}
