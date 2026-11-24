package guest

import (
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
)

// exitObs records the guest's send and halt order.
type exitObs struct {
	mu    sync.Mutex
	order []string
}

func (o *exitObs) add(s string) {
	o.mu.Lock()
	o.order = append(o.order, s)
	o.mu.Unlock()
}

func (o *exitObs) snapshot() []string {
	o.mu.Lock()
	defer o.mu.Unlock()
	out := make([]string, len(o.order))
	copy(out, o.order)
	return out
}

// exitRecorder notes when the Exit frame body is written.
type exitRecorder struct {
	io.ReadWriteCloser
	obs *exitObs
}

func (r *exitRecorder) Write(p []byte) (int, error) {
	n, err := r.ReadWriteCloser.Write(p)
	if err == nil && bytes.Contains(p, []byte(`"method":"Exit"`)) {
		r.obs.add("sent")
	}
	return n, err
}

// gateConn blocks Write while block is set, until Close.
// A host that has stopped reading looks like this: the write does not
// finish, and it must not delay the halt past the send timeout.
type gateConn struct {
	io.ReadWriteCloser
	block  atomic.Bool
	closed chan struct{}
	once   sync.Once
}

func newGateConn(rw io.ReadWriteCloser) *gateConn {
	return &gateConn{ReadWriteCloser: rw, closed: make(chan struct{})}
}

func (g *gateConn) Write(p []byte) (int, error) {
	if g.block.Load() {
		<-g.closed
		return 0, io.ErrClosedPipe
	}
	return g.ReadWriteCloser.Write(p)
}

func (g *gateConn) Close() error {
	g.once.Do(func() { close(g.closed) })
	return g.ReadWriteCloser.Close()
}

func TestWorkloadExitReachesHostBeforeHalt(t *testing.T) {
	leakcheck.Check(t)
	cases := []struct {
		name   string
		argv   []string
		code   int32
		reason string
	}{
		{name: "zero", argv: []string{"/bin/true"}, code: 0, reason: "Completed"},
		{name: "nonzero", argv: []string{"/bin/sh", "-c", "exit 7"}, code: 7, reason: "Error"},
		{name: "signal", argv: []string{"/bin/sh", "-c", "kill -KILL $$"}, code: 128 + 9, reason: "Error"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leakcheck.Check(t)
			a, b := net.Pipe()
			obs := &exitObs{}
			guestConn := &exitRecorder{ReadWriteCloser: a, obs: obs}
			h := &Handler{Token: "secret", AgentVersion: "test"}
			h.Init()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() { _ = Serve(ctx, guestConn, *h) }()

			dialCtx, dialCancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer dialCancel()
			cli, err := Dial(dialCtx, b, "secret", "host")
			if err != nil {
				t.Fatal(err)
			}
			defer cli.Close()

			var got atomic.Value
			cli.OnExit(func(rep ExitReport) { got.Store(rep) })
			Supervise(ctx, h, tc.argv, time.Second, func() { obs.add("halt") })

			deadline := time.Now().Add(2 * time.Second)
			var rep ExitReport
			for time.Now().Before(deadline) {
				v := got.Load()
				if v != nil {
					rep = v.(ExitReport)
					break
				}
				time.Sleep(5 * time.Millisecond)
			}
			if rep.ExitCode == nil {
				t.Fatalf("host did not observe workload exit %d", tc.code)
			}
			if *rep.ExitCode != tc.code || rep.Reason != tc.reason {
				t.Fatalf("exit code %d reason %q, workload reported %+v", tc.code, tc.reason, rep)
			}
			if got := NormalizeExitReason(rep.Reason); got != rep.Reason {
				t.Fatalf("reason %q is not a valid exit reason", rep.Reason)
			}
			order := obs.snapshot()
			if len(order) < 2 || order[0] != "sent" || order[1] != "halt" {
				t.Fatalf("halt ran before the Exit frame was written: %v", order)
			}
		})
	}
}

func TestWorkloadExitHostUnreachableStillHalts(t *testing.T) {
	leakcheck.Check(t)
	a, b := net.Pipe()
	gate := newGateConn(a)
	h := &Handler{Token: "secret", AgentVersion: "test"}
	h.Init()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, gate, *h) }()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer dialCancel()
	cli, err := Dial(dialCtx, b, "secret", "host")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	// The handshake is done. Further writes block until the agent closes
	// the connection, which is what a dead host looks like.
	gate.block.Store(true)

	halted := make(chan struct{})
	start := time.Now()
	done := make(chan struct{})
	go func() {
		Supervise(context.Background(), h, []string{"/bin/true"}, 200*time.Millisecond, func() { close(halted) })
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("dead host connection hung the halt")
	}
	select {
	case <-halted:
	default:
		t.Fatal("dead host connection hung the halt: halt was not called")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("dead host connection hung the halt: took %s", elapsed)
	}
}
