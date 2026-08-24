package guest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	in := Envelope{V: ProtocolVersion, ID: "1", Kind: KindRequest, Method: MethodHealth}
	if err := WriteFrame(&buf, in); err != nil {
		t.Fatal(err)
	}
	out, err := ReadFrame(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != "1" || out.Method != MethodHealth {
		t.Fatalf("got %+v", out)
	}
}

func TestRejectsOversizedLength(t *testing.T) {
	raw := []byte{0xff, 0xff, 0xff, 0xff}
	_, err := ReadFrame(bytes.NewReader(raw))
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestHandshakeAndExec(t *testing.T) {
	a, b := net.Pipe()
	h := Handler{
		Token:        "secret",
		AgentVersion: "test",
		ExecFn: func(ctx context.Context, req ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			if len(req.Argv) == 0 || req.Argv[0] != "true" {
				_, _ = stderr.Write([]byte("bad"))
				return 2, nil
			}
			_, _ = stdout.Write([]byte("ok\n"))
			return 0, nil
		},
		NetInfoFn: func() (NetInfoRes, error) {
			return NetInfoRes{PrimaryIP: "192.168.64.2", IPs: []string{"192.168.64.2"}}, nil
		},
	}
	errCh := make(chan error, 1)
	go func() { errCh <- Serve(context.Background(), a, h) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if _, err := Dial(ctx, b, "wrong", "host"); err == nil {
		t.Fatal("expected unauthorized")
	}
	// pipe is dead after failed handshake (server writes error then continues,
	// but client closed). Use a fresh pair.
	a.Close()
	b.Close()

	a, b = net.Pipe()
	go func() { _ = Serve(context.Background(), a, h) }()
	cli, err := Dial(ctx, b, "secret", "host")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	hs := cli.Info()
	if !hs.OK || hs.AgentVersion != "test" {
		t.Fatalf("handshake %+v", hs)
	}
	health, err := cli.Health(ctx)
	if err != nil || !health.OK {
		t.Fatalf("health %v %v", health, err)
	}
	ready, err := cli.Ready(ctx)
	if err != nil || !ready.Ready {
		t.Fatalf("ready %v %v", ready, err)
	}
	netinfo, err := cli.NetInfo(ctx)
	if err != nil || netinfo.PrimaryIP != "192.168.64.2" {
		t.Fatalf("netinfo %v %v", netinfo, err)
	}
	stdout, _, code, err := cli.Exec(ctx, ExecReq{Argv: []string{"true"}})
	if err != nil || code != 0 || string(stdout) != "ok\n" {
		t.Fatalf("exec stdout=%q code=%d err=%v", stdout, code, err)
	}
	pr, err := cli.Probe(ctx, ProbeReq{Type: ProbeExec, Argv: []string{"true"}})
	if err != nil || !pr.OK {
		t.Fatalf("probe %v %v", pr, err)
	}
}

func TestEmptyTokenHandshakeRejected(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	h := Handler{Token: "", AgentVersion: "test"}
	go func() { _ = Serve(context.Background(), a, h) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := Dial(ctx, b, "anything", "host"); err == nil {
		t.Fatal("empty server token must reject handshake")
	}
}

func TestInsecureEmptyTokenHandshakeAllowed(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	h := Handler{Token: "", AllowInsecureNoToken: true, AgentVersion: "test"}
	go func() { _ = Serve(context.Background(), a, h) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cli, err := Dial(ctx, b, "", "host")
	if err != nil {
		t.Fatal(err)
	}
	_ = cli.Close()
}

func TestMetalAvailableUsesHook(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	h := Handler{
		Token:            "secret",
		AgentVersion:     "test",
		MetalAvailableFn: func() bool { return false },
	}
	go func() { _ = Serve(context.Background(), a, h) }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cli, err := Dial(ctx, b, "secret", "host")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if cli.Info().MetalAvailable {
		t.Fatal("MetalAvailable must not be implied by GOOS")
	}
}

func TestAuthorizeHandshake(t *testing.T) {
	if err := authorizeHandshake(Handler{Token: ""}, HandshakeReq{Token: "x"}); err == nil {
		t.Fatal("empty token")
	}
	if err := authorizeHandshake(Handler{Token: "a"}, HandshakeReq{Token: "b"}); err == nil {
		t.Fatal("mismatch")
	}
	if err := authorizeHandshake(Handler{Token: "a"}, HandshakeReq{Token: "a"}); err != nil {
		t.Fatal(err)
	}
}

func TestCallErrorType(t *testing.T) {
	e := &CallError{Code: "x", Message: "y"}
	if e.Error() != "x: y" {
		t.Fatal(e.Error())
	}
	var err error = e
	if !errors.As(err, new(*CallError)) {
		t.Fatal("errors.As")
	}
}
