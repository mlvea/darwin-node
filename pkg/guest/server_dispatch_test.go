package guest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestHandshakeNonceReplayRejected(t *testing.T) {
	h := Handler{Token: "secret", AgentVersion: "test"}
	h.Init()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a1, b1 := net.Pipe()
	defer a1.Close()
	defer b1.Close()
	go func() { _ = Serve(ctx, a1, h) }()
	s1 := NewSession(b1)
	defer s1.Close()
	if _, err := s1.Call(ctx, MethodHandshake, HandshakeReq{Token: "secret", Nonce: "fixed-nonce"}); err != nil {
		t.Fatalf("first handshake: %v", err)
	}

	a2, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	go func() { _ = Serve(ctx, a2, h) }()
	s2 := NewSession(b2)
	defer s2.Close()
	_, err := s2.Call(ctx, MethodHandshake, HandshakeReq{Token: "secret", Nonce: "fixed-nonce"})
	if err == nil {
		t.Fatal("reused nonce must fail")
	}
	var ce *CallError
	if !errors.As(err, &ce) || ce.Code != ErrCodeUnauthorized || !strings.Contains(ce.Message, "replay") {
		t.Fatalf("want unauthorized/replay, got %v", err)
	}

	a3, b3 := net.Pipe()
	defer a3.Close()
	defer b3.Close()
	go func() { _ = Serve(ctx, a3, h) }()
	cli, err := Dial(ctx, b3, "secret", "host")
	if err != nil {
		t.Fatalf("unique nonce: %v", err)
	}
	_ = cli.Close()
}

func TestHandshakeNonceReplaySameConn(t *testing.T) {
	h := Handler{Token: "secret"}
	h.Init()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _ = Serve(ctx, a, h) }()
	s := NewSession(b)
	defer s.Close()
	if _, err := s.Call(ctx, MethodHandshake, HandshakeReq{Token: "secret", Nonce: "once"}); err != nil {
		t.Fatal(err)
	}
	_, err := s.Call(ctx, MethodHandshake, HandshakeReq{Token: "secret", Nonce: "once"})
	if err == nil {
		t.Fatal("second handshake with same nonce must fail")
	}
	if !strings.Contains(err.Error(), "replay") {
		t.Fatalf("got %v", err)
	}
}

func TestTokenReloadAllowsLaterHandshake(t *testing.T) {
	h := Handler{Token: "", AgentVersion: "test"}
	h.Init()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a1, b1 := net.Pipe()
	go func() { _ = Serve(ctx, a1, h) }()
	if _, err := Dial(ctx, b1, "secret", "host"); err == nil {
		t.Fatal("empty token must reject")
	}
	_ = a1.Close()
	_ = b1.Close()

	h.Token = "secret"
	h.Reload()

	a2, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	go func() { _ = Serve(ctx, a2, h) }()
	cli, err := Dial(ctx, b2, "secret", "host")
	if err != nil {
		t.Fatalf("after reload: %v", err)
	}
	_ = cli.Close()
}

func TestWatchTokenFileHeals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-token")
	h := Handler{Token: ""}
	h.Init()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.WatchTokenFile(ctx, path, 20*time.Millisecond)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.CurrentToken() != "" {
			t.Fatal("token appeared before file")
		}
		time.Sleep(10 * time.Millisecond)
		break
	}

	if err := os.WriteFile(path, []byte("later-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if h.CurrentToken() == "later-secret" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("token not loaded, got %q", h.CurrentToken())
}

func TestLimiterCaps(t *testing.T) {
	l := NewLimiter(2, 1)
	if !l.TryConn() || !l.TryConn() || l.TryConn() {
		t.Fatal("conn cap")
	}
	l.ReleaseConn()
	if !l.TryConn() {
		t.Fatal("conn release")
	}
	if !l.TryExec() || l.TryExec() {
		t.Fatal("exec cap")
	}
	l.ReleaseExec()
	if !l.TryExec() {
		t.Fatal("exec release")
	}
}

func TestConnLimitOverloaded(t *testing.T) {
	h := Handler{Token: "secret", Limiter: NewLimiter(1, 4)}
	h.Init()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	a1, b1 := net.Pipe()
	defer a1.Close()
	defer b1.Close()
	go func() { _ = Serve(ctx, a1, h) }()
	cli, err := Dial(ctx, b1, "secret", "host")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	a2, b2 := net.Pipe()
	defer a2.Close()
	defer b2.Close()
	go func() { _ = Serve(ctx, a2, h) }()
	_, err = Dial(ctx, b2, "secret", "host")
	if err == nil {
		t.Fatal("expected overloaded")
	}
	var ce *CallError
	if !errors.As(err, &ce) || ce.Code != ErrCodeOverloaded {
		t.Fatalf("got %v", err)
	}
}

func TestExecLimitOverloaded(t *testing.T) {
	started := make(chan struct{}, 8)
	block := make(chan struct{})
	h := Handler{
		Token:   "secret",
		Limiter: NewLimiter(8, 2),
		ExecFn: func(ctx context.Context, req ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			started <- struct{}{}
			select {
			case <-block:
			case <-ctx.Done():
			}
			return 0, nil
		},
	}
	h.Init()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() { _ = Serve(ctx, a, h) }()
	cli, err := Dial(ctx, b, "secret", "host")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cli.ExecStream(ctx, ExecReq{Argv: []string{"hold"}}, io.Discard, io.Discard)
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("execs did not start")
		}
	}
	_, err = cli.ExecStream(ctx, ExecReq{Argv: []string{"extra"}}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("expected overloaded")
	}
	var ce *CallError
	if !errors.As(err, &ce) || ce.Code != ErrCodeOverloaded {
		t.Fatalf("got %v", err)
	}
	close(block)
	wg.Wait()
}

func TestServeHealthNotBlockedBySlowProbe(t *testing.T) {
	inProbe := make(chan struct{})
	h := Handler{
		Token: "secret",
		ProbeHTTPFn: func(ctx context.Context, req ProbeReq) ProbeRes {
			close(inProbe)
			select {
			case <-ctx.Done():
				return ProbeRes{OK: false, Message: ctx.Err().Error()}
			case <-time.After(2 * time.Second):
				return ProbeRes{OK: true}
			}
		},
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	go func() { _ = Serve(ctx, a, h) }()
	cli, err := Dial(ctx, b, "secret", "host")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	errCh := make(chan error, 1)
	go func() {
		_, err := cli.Probe(ctx, ProbeReq{Type: ProbeHTTP, Timeout: 5 * time.Second})
		errCh <- err
	}()
	select {
	case <-inProbe:
	case <-time.After(2 * time.Second):
		t.Fatal("probe did not start")
	}
	start := time.Now()
	health, err := cli.Health(ctx)
	if err != nil || !health.OK {
		t.Fatalf("health %v %v", health, err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("health took %s; Serve is still serial", d)
	}
}

func TestRunProbeTimeoutClamped(t *testing.T) {
	h := Handler{
		ProbeHTTPFn: func(ctx context.Context, req ProbeReq) ProbeRes {
			d, ok := ctx.Deadline()
			if !ok {
				t.Error("missing deadline")
				return ProbeRes{}
			}
			remain := time.Until(d)
			if remain > MaxProbeTimeout {
				t.Errorf("deadline %s exceeds cap", remain)
			}
			if remain < MaxProbeTimeout-5*time.Second {
				t.Errorf("deadline %s too short", remain)
			}
			return ProbeRes{OK: true}
		},
	}
	res := h.runProbe(context.Background(), ProbeReq{Type: ProbeHTTP, Timeout: 24 * time.Hour})
	if !res.OK {
		t.Fatalf("%+v", res)
	}
}

func TestLogBufferCapsLineAndBudget(t *testing.T) {
	b := NewLogBuffer(1_000_000)
	line := bytes.Repeat([]byte("A"), 1<<20)
	if _, err := b.Write(line); err != nil {
		t.Fatal(err)
	}
	first := b.Tail(0)
	if len(first) != (1<<20)/MaxLogLineBytes {
		t.Fatalf("split lines=%d", len(first))
	}
	for _, ln := range first {
		if len(ln) > MaxLogLineBytes {
			t.Fatalf("line %d exceeds cap", len(ln))
		}
	}

	chunk := bytes.Repeat([]byte("B"), 64<<10)
	for i := 0; i < 200; i++ {
		if _, err := b.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	var total int
	for _, ln := range b.Tail(0) {
		if len(ln) > MaxLogLineBytes {
			t.Fatalf("line %d", len(ln))
		}
		total += len(ln)
	}
	if total > DefaultLogBufferBytes {
		t.Fatalf("ring bytes %d > budget %d", total, DefaultLogBufferBytes)
	}
	if total < DefaultLogBufferBytes/2 {
		t.Fatalf("ring bytes %d unexpectedly small", total)
	}
}

func TestExecStreamAndExecCap(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 4<<20)
	h := Handler{
		Token: "secret",
		ExecFn: func(ctx context.Context, req ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			_, err := stdout.Write(payload)
			return 0, err
		},
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go func() { _ = Serve(ctx, a, h) }()
	cli, err := Dial(ctx, b, "secret", "host")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	var got bytes.Buffer
	exit, err := cli.ExecStream(ctx, ExecReq{Argv: []string{"cat"}}, &got, io.Discard)
	if err != nil || exit != 0 {
		t.Fatalf("stream exit=%d err=%v", exit, err)
	}
	if got.Len() != len(payload) {
		t.Fatalf("stream got %d want %d", got.Len(), len(payload))
	}

	stdout, _, code, err := cli.Exec(ctx, ExecReq{Argv: []string{"cat"}})
	if err != nil || code != 0 {
		t.Fatalf("exec code=%d err=%v", code, err)
	}
	if len(stdout) != MaxExecCapture {
		t.Fatalf("exec cap got %d want %d", len(stdout), MaxExecCapture)
	}
}

func TestIdleConnsFreeLimiter(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	h := Handler{
		Token:            "secret",
		HandshakeTimeout: 200 * time.Millisecond,
		IdleTimeout:      200 * time.Millisecond,
		Limiter:          NewLimiter(8, 4),
	}
	h.Init()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) { _ = Serve(ctx, c, h) }(c)
		}
	}()

	idle := make([]net.Conn, 8)
	for i := range idle {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		idle[i] = c
	}
	t.Cleanup(func() {
		for _, c := range idle {
			if c != nil {
				_ = c.Close()
			}
		}
	})

	time.Sleep(500 * time.Millisecond)

	dctx, dcancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer dcancel()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	cli, err := Dial(dctx, raw, "secret", "host")
	if err != nil {
		t.Fatalf("9th connection after idle window: %v", err)
	}
	_ = cli.Close()
}
