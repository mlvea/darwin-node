package engine

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// trackConn records Close. net.Pipe has no finalizer, so a client that is
// dropped stays open for the life of the process.
type trackConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *trackConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// readyFailRuntime's DialAgent completes the handshake, then returns a
// protocol error for Ready and leaves the connection open. Stop does not
// own that connection: only Client.Close does.
type readyFailRuntime struct {
	inner  *fake.Runtime
	closed chan struct{}
}

func (r *readyFailRuntime) Name() types.RuntimeName { return r.inner.Name() }

func (r *readyFailRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.inner.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &readyFailMachine{Machine: m, token: spec.AgentToken, closed: r.closed}, nil
}

type readyFailMachine struct {
	runtime.Machine
	token  string
	closed chan struct{}
}

func (m *readyFailMachine) DialAgent(ctx context.Context) (*guest.Client, error) {
	a, b := net.Pipe()
	tracked := &trackConn{Conn: b, closed: m.closed}
	go serveReadyError(a)
	return guest.Dial(ctx, tracked, m.token, "test-host")
}

func serveReadyError(rw io.ReadWriteCloser) {
	defer rw.Close()
	env, err := guest.ReadFrame(rw)
	if err != nil {
		return
	}
	payload, err := guest.EncodePayload(guest.HandshakeRes{OK: true, AgentVersion: "test", Protocol: guest.ProtocolVersion})
	if err != nil {
		return
	}
	if err := guest.WriteFrame(rw, guest.Envelope{V: guest.ProtocolVersion, ID: env.ID, Kind: guest.KindResponse, Payload: payload}); err != nil {
		return
	}
	env, err = guest.ReadFrame(rw)
	if err != nil {
		return
	}
	_ = guest.WriteFrame(rw, guest.Envelope{
		V: guest.ProtocolVersion, ID: env.ID, Kind: guest.KindError,
		Error: &guest.CallError{Code: "down", Message: "guest not ready"},
	})
	// Stay in Read until the host closes. Returning here would close the
	// pipe and hide a missing Client.Close.
	_, _ = guest.ReadFrame(rw)
}

func TestReadyFailureClosesAgentConn(t *testing.T) {
	leakcheck.Check(t)
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 2 * time.Second
	cfg.AllowNATWorkloads = true
	closed := make(chan struct{})
	e := New(cfg, slots, &readyFailRuntime{inner: fake.New(), closed: closed}, sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)
	if err := e.Create(context.Background(), samplePod("rdy", "uid-rdy"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "rdy", corev1.PodFailed)
	if p.Status.Message == "" {
		t.Fatal("failed pod missing message")
	}
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("guest agent connection still open after Ready failure")
	}
	if used := e.slots.Used(); used != 0 {
		t.Fatalf("slot leak after Ready failure: %d", used)
	}
}
