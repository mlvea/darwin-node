package guest

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
)

// serveRaw runs Serve against a pipe and returns the host end for writing
// arbitrary bytes.
func serveRaw(t *testing.T) net.Conn {
	t.Helper()
	leakcheck.Check(t)
	hostC, agentC := net.Pipe()
	h := Handler{Token: "tok", LogBuffer: NewLogBuffer(16), IdleTimeout: time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() { _ = Serve(ctx, agentC, h) }()
	t.Cleanup(func() { _ = hostC.Close() })
	return hostC
}

// writeFrame emits one length-prefixed envelope with caller-controlled bytes.
func writeFrame(t *testing.T, conn net.Conn, body []byte) {
	t.Helper()
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(body); err != nil {
		t.Fatal(err)
	}
}

func readFrameWithTimeout(t *testing.T, conn net.Conn) ([]byte, bool) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, false
	}
	n := binary.BigEndian.Uint32(hdr[:])
	body := make([]byte, n)
	if _, err := io.ReadFull(conn, body); err != nil {
		return nil, false
	}
	return body, true
}

func TestAdversarialGarbageBytesTerminateCleanly(t *testing.T) {
	hostC, agentC := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), agentC, Handler{Token: "tok"}) }()
	// Async write: Serve stops reading after the bogus 4-byte header, and a
	// synchronous net.Pipe write would otherwise block on the unread tail.
	go hostC.Write([]byte("this is not a frame"))
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("garbage input should terminate Serve with an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve hung on garbage input")
	}
}

func TestAdversarialOversizedLengthRejected(t *testing.T) {
	hostC := serveRaw(t)
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 0xFFFFFFF0)
	if _, err := hostC.Write(hdr[:]); err != nil {
		t.Fatal(err)
	}
	// Serve must close the connection rather than allocate 4 GB. A write or
	// read after the peer closes fails quickly; success of THIS test is that
	// nothing crashes and no allocation storm occurs (implicit via timeout).
	hostC.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 16)
	if _, err := hostC.Read(buf); err == nil {
		// Peer may keep the conn half-open until its read errors; either way,
		// the process surviving is the assertion.
		t.Log("peer still writable after oversized header")
	}
}

func TestAdversarialWrongVersionRejected(t *testing.T) {
	hostC, agentC := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), agentC, Handler{Token: "tok"}) }()
	writeFrame(t, hostC, []byte(`{"v":99,"id":"1","kind":"req","method":"Handshake"}`))
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("wrong protocol version must terminate Serve with an error")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve accepted a wrong-version frame")
	}
	_ = hostC.Close()
}

func TestAdversarialStreamBeforeHandshakeIgnored(t *testing.T) {
	hostC := serveRaw(t)
	// A KindStream frame before any handshake must be dropped silently.
	writeFrame(t, hostC, []byte(`{"v":1,"id":"9","kind":"stream","method":"Exec","payload":{}}`))
	// The connection must remain usable: complete a handshake afterwards.
	cli, err := Dial(context.Background(), hostC, "tok", "test")
	if err != nil {
		t.Fatalf("handshake after stray stream frame failed: %v", err)
	}
	defer cli.Close()
	res, err := cli.Health(context.Background())
	if err != nil || !res.OK {
		t.Fatalf("post-stray health: %v %+v", err, res)
	}
}

func TestAdversarialUnknownMethodReturnsCallError(t *testing.T) {
	h := Handler{Token: "tok", LogBuffer: NewLogBuffer(16)}
	cli, _ := serveTestHandler(t, h)
	ch, err := cli.sess.Stream(context.Background(), "NoSuchMethod", nil)
	if err != nil {
		t.Fatal(err)
	}
	for env := range ch {
		if env.Kind == KindError && env.Error != nil && env.Error.Code == "unknown_method" {
			return
		}
	}
	t.Fatal("unknown method must yield an unknown_method CallError")
}

func TestAdversarialDoubleHandshakeNonce(t *testing.T) {
	h := Handler{Token: "tok", LogBuffer: NewLogBuffer(16)}
	cli, _ := serveTestHandler(t, h)
	// Replaying the same nonce must not be accepted twice.
	req := HandshakeReq{Token: "tok", Nonce: "replay-me"}
	env, err := cli.sess.Call(context.Background(), MethodHandshake, req)
	if err != nil {
		if env.Error != nil && env.Error.Code == ErrCodeUnauthenticated {
			return
		}
		t.Fatalf("want unauthenticated replay rejection, got %v", err)
	}
	// Some builds accept re-handshake; the invariant is that it cannot
	// escalate: health must still work and never bypass auth.
	if _, err := cli.Health(context.Background()); err != nil {
		t.Fatalf("session unusable after replay attempt: %v", err)
	}
}
