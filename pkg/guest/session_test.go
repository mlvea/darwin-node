package guest

import (
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func waitPending(t *testing.T, s *Session, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := s.pendingCount(); got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("pendingCount=%d, want %d", s.pendingCount(), want)
		}
		time.Sleep(time.Millisecond)
	}
}

func drainConn(t *testing.T, c net.Conn) {
	t.Helper()
	fc := NewFrameConn(c)
	go func() {
		for {
			if _, err := fc.Read(); err != nil {
				return
			}
		}
	}()
}

func TestSessionStreamCancelClearsPending(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	drainConn(t, b)

	sess := NewSession(a)
	defer sess.Close()

	const n = 100
	cancels := make([]context.CancelFunc, 0, n)
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		cancels = append(cancels, cancel)
		if _, err := sess.Stream(ctx, MethodLogs, nil); err != nil {
			t.Fatalf("Stream: %v", err)
		}
	}
	if got := sess.pendingCount(); got != n {
		t.Fatalf("pending before cancel = %d, want %d", got, n)
	}
	for _, cancel := range cancels {
		cancel()
	}
	waitPending(t, sess, 0)
}

func TestSessionCallWriteErrorClearsPending(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	sess := NewSession(a)
	defer sess.Close()

	_ = b.Close()
	select {
	case <-sess.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not observe closed peer")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := sess.Call(ctx, MethodHealth, nil)
	if err == nil {
		t.Fatal("expected Call error on closed peer")
	}
	if got := sess.pendingCount(); got != 0 {
		t.Fatalf("pending after failed Call = %d, want 0", got)
	}
}

func TestSessionStreamWriteErrorClearsPending(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	sess := NewSession(a)
	defer sess.Close()

	_ = b.Close()
	select {
	case <-sess.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not observe closed peer")
	}

	_, err := sess.Stream(context.Background(), MethodLogs, nil)
	if err == nil {
		t.Fatal("expected Stream error on closed peer")
	}
	if got := sess.pendingCount(); got != 0 {
		t.Fatalf("pending after failed Stream = %d, want 0", got)
	}
}

func TestSessionSlowStreamDoesNotBlockCall(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	peer := NewFrameConn(b)
	go func() {
		for {
			env, err := peer.Read()
			if err != nil {
				return
			}
			switch env.Method {
			case MethodLogs:
				id := env.ID
				go func() {
					payload, _ := EncodePayload(LogsEvent{Line: []byte("x")})
					for i := 0; i < 128; i++ {
						if err := peer.Write(Envelope{
							V: ProtocolVersion, ID: id, Kind: KindStream, Payload: payload,
						}); err != nil {
							return
						}
					}
				}()
			case MethodHealth:
				payload, _ := EncodePayload(HealthRes{OK: true})
				_ = peer.Write(Envelope{
					V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Payload: payload,
				})
			}
		}
	}()

	sess := NewSession(a)
	defer sess.Close()

	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	out, err := sess.Stream(streamCtx, MethodLogs, LogsReq{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	_ = out // never drain: consumer too slow

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	env, err := sess.Call(ctx, MethodHealth, nil)
	if err != nil {
		t.Fatalf("Call blocked or failed: %v", err)
	}
	if env.Kind != KindResponse {
		t.Fatalf("Call kind=%s, want %s", env.Kind, KindResponse)
	}
	got, err := Decode[HealthRes](env)
	if err != nil || !got.OK {
		t.Fatalf("health %+v err=%v", got, err)
	}
}

func TestSessionCallCancelClearsPending(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	drainConn(t, b)

	sess := NewSession(a)
	defer sess.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := sess.Call(ctx, MethodHealth, nil)
		errCh <- err
	}()
	waitPending(t, sess, 1)
	cancel()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected cancel error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call did not return after cancel")
	}
	waitPending(t, sess, 0)
}

func TestSessionClosedReturnsEOF(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	sess := NewSession(a)
	if err := sess.Close(); err != nil && err != io.EOF {
		t.Fatal(err)
	}
	select {
	case <-sess.closed:
	case <-time.After(2 * time.Second):
		t.Fatal("readLoop did not exit")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := sess.Call(ctx, MethodHealth, nil)
	if err == nil {
		t.Fatal("expected error after Close")
	}
	if sess.pendingCount() != 0 {
		t.Fatalf("pending=%d", sess.pendingCount())
	}
}
