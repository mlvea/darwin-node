package guest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// enteredConn reports when a write is blocked inside the framed write so a
// later call can be started only after the connection mutex is held.
type enteredConn struct {
	net.Conn
	entered chan struct{}
	once    chan struct{}
}

func newEnteredConn(c net.Conn) *enteredConn {
	return &enteredConn{Conn: c, entered: make(chan struct{})}
}

func (c *enteredConn) Write(p []byte) (int, error) {
	if c.once == nil {
		c.once = make(chan struct{})
		close(c.entered)
	}
	return c.Conn.Write(p)
}

// A host session must arm a write deadline. Without one, a guest that stops
// reading leaves Call blocked inside Write and pod delete never reaches Close.
func TestHostSessionWriteDeadlineIsArmed(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	s := NewSession(a)
	defer s.Close()
	if s.conn.writeTimeout != DefaultWriteTimeout {
		t.Fatalf("host write timeout %s, want %s", s.conn.writeTimeout, DefaultWriteTimeout)
	}
}

// The deadline has to fire even when the caller passed a context with no
// deadline. Otherwise a long termination grace still pins the VM slot.
func TestCallWriteDeadlineFiresWithoutContextDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	s := NewSession(a)
	defer s.Close()
	if s.conn.writeTimeout <= 0 {
		t.Fatal("host session has no write deadline")
	}
	s.conn.writeTimeout = 150 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := s.Call(context.Background(), MethodShutdown, ShutdownReq{Reason: "pod deleted"})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a write error once the guest stopped reading")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call blocked: guest that stopped reading held the host write")
	}
}

// A call context must abort the write before the session write deadline.
// Pod delete's grace is often shorter than that deadline.
func TestCallWriteObeysContextWhenGuestStopsReading(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	s := NewSession(a)
	defer s.Close()
	// A long session deadline must not outrank the call context.
	s.conn.writeTimeout = time.Minute

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := s.Call(ctx, MethodShutdown, ShutdownReq{Reason: "pod deleted"})
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Call err %v, want context deadline", err)
		}
		if time.Since(start) > 2*time.Second {
			t.Fatalf("Call returned in %s, context was 300ms", time.Since(start))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Call blocked after the guest stopped reading")
	}
	if s.pendingCount() != 0 {
		t.Fatalf("pending=%d after the timed-out call", s.pendingCount())
	}
}

// Dial is the host path. A guest that never reads the handshake must not
// pin the dial until the process restarts.
func TestDialWriteObeysContextWhenGuestStopsReading(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := Dial(ctx, a, "tok", "host")
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Dial err %v, want context deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Dial blocked after the guest stopped reading")
	}
}

// A write already stuck on the session mutex must not keep a later call
// (pod shutdown) blocked past that call's context.
func TestLaterCallContextUnblocksStuckWrite(t *testing.T) {
	raw, guest := net.Pipe()
	defer guest.Close()
	host := newEnteredConn(raw)
	s := NewSession(host)
	defer s.Close()
	s.conn.writeTimeout = time.Minute

	first := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		_, err := s.Call(ctx, MethodHealth, nil)
		first <- err
	}()
	select {
	case <-host.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first write did not start")
	}

	second := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, err := s.Call(ctx, MethodShutdown, nil)
		second <- err
	}()
	select {
	case err := <-second:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("later call err %v, want context deadline", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("later call blocked behind a stuck guest write")
	}
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("stuck write did not return after the later call gave up")
	}
}

// callByteLimit is the retained-byte ceiling the host must enforce for one
// logs or exec stream. A guest flood has to fail the call here instead of
// growing the heap until the final response arrives.
const callByteLimit = 8 << 20

func floodLogLines(t *testing.T, peer *FrameConn, id string, total int) {
	t.Helper()
	line := bytes.Repeat([]byte("a"), 64<<10)
	payload, err := EncodePayload(LogsEvent{Line: line})
	if err != nil {
		t.Fatal(err)
	}
	sent := 0
	for sent < total {
		if err := peer.Write(Envelope{
			V: ProtocolVersion, ID: id, Kind: KindStream, Payload: payload,
		}); err != nil {
			return
		}
		sent += len(line)
	}
}

// kubectl logs collects every frame into one slice and does not return
// until the guest sends a final response. A guest that never stops can
// grow the host heap without a bound.
func TestLogsFailsWhenGuestFloodsPastCallLimit(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	peer := NewFrameConn(b)
	sess := NewSession(a)
	defer sess.Close()
	cli := &Client{sess: sess}

	go func() {
		env, err := peer.Read()
		if err != nil {
			return
		}
		floodLogLines(t, peer, env.ID, callByteLimit+(1<<20))
		_ = peer.Write(Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: MethodLogs})
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	lines, err := cli.Logs(ctx, LogsReq{})
	var got int
	for _, ln := range lines {
		got += len(ln)
	}
	if err == nil {
		t.Fatalf("logs retained %d bytes until the final response, no limit error", got)
	}
	if !strings.Contains(err.Error(), "limit") {
		t.Fatalf("logs err %v, want a limit error", err)
	}
	if got > callByteLimit {
		t.Fatalf("logs retained %d bytes after the limit error", got)
	}
}

// Frames the caller has not read yet are still host memory. Filling the
// 32-slot stream channel with large payloads has to fail the call once the
// byte cap is hit, not keep every frame.
func TestUnreadStreamErrorsAtByteLimit(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	peer := NewFrameConn(b)
	sess := NewSession(a)
	defer sess.Close()

	started := make(chan struct{})
	sent := make(chan struct{})
	go func() {
		env, err := peer.Read()
		if err != nil {
			return
		}
		close(started)
		line := bytes.Repeat([]byte("b"), 512<<10)
		payload, err := EncodePayload(LogsEvent{Line: line})
		if err != nil {
			return
		}
		// 20 frames is 10 MiB and still under the 32-frame stream buffer,
		// so a frame-count cap does not save the host. The caller does not
		// read until every write has returned, so a cap that only works
		// when the consumer keeps up cannot pass.
		for i := 0; i < 20; i++ {
			if err := peer.Write(Envelope{
				V: ProtocolVersion, ID: env.ID, Kind: KindStream, Payload: payload,
			}); err != nil {
				return
			}
		}
		_ = peer.Write(Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: MethodLogs})
		close(sent)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := sess.Stream(ctx, MethodLogs, LogsReq{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("guest did not see the logs request")
	}
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("guest did not finish the flood before the host read it")
	}

	var got int
	deadline := time.After(3 * time.Second)
	for {
		select {
		case env, ok := <-out:
			if !ok {
				t.Fatalf("stream ended with %d bytes and no limit error", got)
			}
			if env.Error != nil {
				if !strings.Contains(env.Error.Error(), "limit") {
					t.Fatalf("stream err %v, want a limit error", env.Error)
				}
				if got > callByteLimit {
					t.Fatalf("delivered %d bytes before the limit error", got)
				}
				return
			}
			if env.Kind == KindStream {
				ev, decErr := Decode[LogsEvent](env)
				if decErr == nil {
					got += len(ev.Line)
				}
			}
			if env.Kind == KindResponse && got > callByteLimit {
				t.Fatalf("unread stream retained %d bytes and completed", got)
			}
		case <-deadline:
			t.Fatalf("timed out after %d bytes with no limit error", got)
		}
	}
}

// A reader that pauses longer than the old 200ms grace must keep the
// stream, including every frame already sent. Dropping it ended kubectl
// logs -f with no error.
func TestSlowStreamKeepsFramesPastGrace(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	peer := NewFrameConn(b)
	sess := NewSession(a)
	defer sess.Close()

	// Two buffers of streamBuf (the session queue and the forward
	// channel) absorb 64 frames. Past that the 200ms grace drops the
	// stream.
	const frames = streamBuf*2 + 16
	sent := make(chan struct{})
	go func() {
		env, err := peer.Read()
		if err != nil {
			return
		}
		payload, _ := EncodePayload(LogsEvent{Line: []byte("line")})
		for i := 0; i < frames; i++ {
			if err := peer.Write(Envelope{
				V: ProtocolVersion, ID: env.ID, Kind: KindStream, Payload: payload,
			}); err != nil {
				return
			}
		}
		if err := peer.Write(Envelope{
			V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: MethodLogs,
		}); err != nil {
			return
		}
		close(sent)
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out, err := sess.Stream(ctx, MethodLogs, LogsReq{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("guest could not finish the stream; host stopped reading")
	}
	time.Sleep(500 * time.Millisecond)

	got := 0
	sawResponse := false
	deadline := time.After(3 * time.Second)
	for !sawResponse {
		select {
		case env, ok := <-out:
			if !ok {
				t.Fatalf("stream closed after %d/%d frames", got, frames)
			}
			if env.Error != nil {
				t.Fatalf("stream error after a %s pause: %v", 500*time.Millisecond, env.Error)
			}
			if env.Kind == KindStream {
				got++
			}
			if env.Kind == KindResponse {
				sawResponse = true
			}
		case <-deadline:
			t.Fatalf("timed out with %d/%d frames", got, frames)
		}
	}
	if got != frames {
		t.Fatalf("got %d frames, want %d; a slow reader lost the stream", got, frames)
	}
}

// One paused stream must not stop another call on the session, and it must
// still deliver its own frames after the pause.
func TestPausedStreamDoesNotStallSibling(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	peer := NewFrameConn(b)
	sess := NewSession(a)
	defer sess.Close()

	const frames = streamBuf*2 + 16
	go func() {
		for {
			env, err := peer.Read()
			if err != nil {
				return
			}
			if env.Kind == KindCancel {
				continue
			}
			switch env.Method {
			case MethodLogs:
				id := env.ID
				go func() {
					payload, _ := EncodePayload(LogsEvent{Line: []byte("x")})
					for i := 0; i < frames; i++ {
						if err := peer.Write(Envelope{
							V: ProtocolVersion, ID: id, Kind: KindStream, Payload: payload,
						}); err != nil {
							return
						}
					}
					_ = peer.Write(Envelope{
						V: ProtocolVersion, ID: id, Kind: KindResponse, Method: MethodLogs,
					})
				}()
			case MethodHealth:
				payload, _ := EncodePayload(HealthRes{OK: true})
				_ = peer.Write(Envelope{
					V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: MethodHealth, Payload: payload,
				})
			}
		}
	}()

	streamCtx, streamCancel := context.WithCancel(context.Background())
	defer streamCancel()
	out, err := sess.Stream(streamCtx, MethodLogs, LogsReq{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	env, err := sess.Call(ctx, MethodHealth, nil)
	if err != nil {
		t.Fatalf("health blocked behind a paused stream: %v", err)
	}
	if env.Kind != KindResponse {
		t.Fatalf("health kind %s", env.Kind)
	}

	got := 0
	deadline := time.After(3 * time.Second)
	for {
		select {
		case env, ok := <-out:
			if !ok {
				t.Fatalf("paused stream closed after %d/%d frames", got, frames)
			}
			if env.Error != nil {
				t.Fatalf("paused stream: %v", env.Error)
			}
			if env.Kind == KindStream {
				got++
			}
			if env.Kind == KindResponse {
				if got != frames {
					t.Fatalf("paused stream delivered %d/%d frames", got, frames)
				}
				return
			}
		case <-deadline:
			t.Fatalf("timed out with %d/%d frames", got, frames)
		}
	}
}

// logLimitPayload is one valid JSON frame. Its length is the call budget in
// TestSessionByteLimitFailsOnlyTheCallThatExceedsIt.
var logLimitPayload = func() json.RawMessage {
	b, err := EncodePayload(strings.Repeat("q", 2048))
	if err != nil {
		panic(err)
	}
	return b
}()

func TestSessionByteLimitFailsOnlyTheCallThatExceedsIt(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	peer := NewFrameConn(b)
	sess := NewSession(a)
	defer sess.Close()
	// One frame fills a call. The second frame fits in its own call budget
	// and overflows the session budget.
	sess.SetLimits(len(logLimitPayload), len(logLimitPayload)+len(logLimitPayload)/2)

	wrote := make(chan struct{})
	go func() {
		n := 0
		for {
			env, err := peer.Read()
			if err != nil {
				return
			}
			if env.Kind == KindCancel || env.Kind == KindWindow {
				continue
			}
			switch env.Method {
			case MethodLogs:
				id := env.ID
				_ = peer.Write(Envelope{V: ProtocolVersion, ID: id, Kind: KindStream, Payload: logLimitPayload})
				n++
				if n == 2 {
					close(wrote)
				}
			case MethodHealth:
				body, _ := EncodePayload(HealthRes{OK: true})
				_ = peer.Write(Envelope{
					V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: MethodHealth, Payload: body,
				})
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	id1, first, err := sess.StreamWithID(ctx, MethodLogs, LogsReq{})
	if err != nil {
		t.Fatal(err)
	}
	id2, second, err := sess.StreamWithID(ctx, MethodLogs, LogsReq{})
	if err != nil {
		t.Fatal(err)
	}
	_ = id1
	select {
	case <-wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("guest did not write both frames before the host read them")
	}
	// The peer's Write returns as soon as the bytes are copied, which can
	// be before dispatch charges them. Wait until the second call has
	// dropped out of pending before anyone reads and releases the first.
	waitLimit := time.After(2 * time.Second)
	for {
		sess.mu.Lock()
		_, live := sess.pending[id2]
		sess.mu.Unlock()
		if !live {
			break
		}
		select {
		case <-waitLimit:
			t.Fatal("second stream stayed open; the session cap did not fail it")
		case <-time.After(5 * time.Millisecond):
		}
	}
	sawSessionLimit := false
	kept := false
	for _, out := range []<-chan Envelope{first, second} {
		select {
		case env, ok := <-out:
			if !ok {
				t.Fatal("stream closed before a frame")
			}
			if env.Error != nil && strings.Contains(env.Error.Error(), "session byte limit") {
				sawSessionLimit = true
				continue
			}
			if env.Kind == KindStream && len(env.Payload) > 0 {
				kept = true
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for a stream frame")
		}
	}
	if !sawSessionLimit {
		t.Fatal("second stream did not fail with a session byte limit error")
	}
	if !kept {
		t.Fatal("the stream that fit in the session budget was dropped too")
	}
	hctx, hcancel := context.WithTimeout(context.Background(), time.Second)
	defer hcancel()
	if _, err := sess.Call(hctx, MethodHealth, nil); err != nil {
		t.Fatalf("health failed after the session limit: %v", err)
	}
}

// A reader that pauses must not lose exec output just because the guest
// produced more than the call buffer. The guest waits for credit. The
// unread buffer stays inside the cap, and a health call still completes.
func TestSlowReaderFlowControlPreservesExecBytes(t *testing.T) {
	const (
		chunk = 4 << 10
		n     = 32
		limit = 16 << 10
	)
	want := bytes.Repeat([]byte("z"), chunk*n)
	h := Handler{
		Token: "tok",
		ExecFn: func(ctx context.Context, req ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			buf := bytes.Repeat([]byte("z"), chunk)
			for i := 0; i < n; i++ {
				if _, err := stdout.Write(buf); err != nil {
					return 1, err
				}
			}
			return 0, nil
		},
	}
	h.Init()
	hostC, agentC := net.Pipe()
	defer hostC.Close()
	go func() { _ = Serve(context.Background(), agentC, h) }()
	cli, err := Dial(context.Background(), hostC, "tok", "test", HostConfig{
		MaxCallBytes:    limit,
		MaxSessionBytes: limit * 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	gate := make(chan struct{})
	out := &gateWriter{gate: gate}
	errCh := make(chan error, 1)
	go func() {
		_, execErr := cli.ExecStream(context.Background(), ExecReq{Argv: []string{"cat"}}, out, io.Discard)
		errCh <- execErr
	}()

	time.Sleep(300 * time.Millisecond)
	hw := cli.sess.bufferHighWater()
	if hw == 0 {
		t.Fatal("exec produced no buffered bytes while the reader was paused")
	}
	if hw > limit {
		t.Fatalf("unread exec bytes high-water %d, cap is %d", hw, limit)
	}
	hctx, hcancel := context.WithTimeout(context.Background(), time.Second)
	defer hcancel()
	if _, err := cli.Health(hctx); err != nil {
		t.Fatalf("health blocked behind a paused exec: %v", err)
	}
	close(gate)

	select {
	case execErr := <-errCh:
		if execErr != nil {
			t.Fatalf("exec: %v", execErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec did not finish after the reader resumed")
	}
	if got := out.String(); got != string(want) {
		t.Fatalf("exec delivered %d bytes, want %d", len(got), len(want))
	}
	if hw := cli.sess.bufferHighWater(); hw > limit {
		t.Fatalf("unread exec bytes high-water %d after resume, cap is %d", hw, limit)
	}
}

// Closing the host while the guest is parked on credit must let Serve
// return. The handler used to stay blocked after the connection was gone.
func TestServeReturnsWhenHostClosesDuringFlowControl(t *testing.T) {
	const limit = 16 << 10
	h := Handler{
		Token: "tok",
		ExecFn: func(ctx context.Context, req ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			buf := bytes.Repeat([]byte("z"), 4<<10)
			for ctx.Err() == nil {
				if _, err := stdout.Write(buf); err != nil {
					return 1, err
				}
			}
			return 0, nil
		},
	}
	h.Init()
	hostC, agentC := net.Pipe()
	served := make(chan struct{})
	go func() {
		_ = Serve(context.Background(), agentC, h)
		close(served)
	}()
	cli, err := Dial(context.Background(), hostC, "tok", "test", HostConfig{
		MaxCallBytes:    limit,
		MaxSessionBytes: limit * 4,
	})
	if err != nil {
		t.Fatal(err)
	}
	gate := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		_, execErr := cli.ExecStream(context.Background(), ExecReq{Argv: []string{"cat"}}, &gateWriter{gate: gate}, io.Discard)
		errCh <- execErr
	}()
	deadline := time.Now().Add(2 * time.Second)
	for cli.sess.bufferHighWater() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if cli.sess.bufferHighWater() == 0 {
		t.Fatal("exec produced no buffered bytes before the host closed")
	}
	_ = cli.Close()
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after the host closed a paused stream")
	}
	close(gate)
	select {
	case <-errCh:
	case <-time.After(2 * time.Second):
		t.Fatal("exec did not return after the host closed")
	}
}

// gateWriter blocks the first Write until gate is closed, then records bytes.
type gateWriter struct {
	gate chan struct{}
	once sync.Once
	mu   sync.Mutex
	buf  bytes.Buffer
}

func (w *gateWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { <-w.gate })
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *gateWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}
