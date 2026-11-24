package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// streamBuf is the old frame-count buffer. Tests still refer to it as the
// size that used to drop a slow reader. Delivery is now bounded in bytes.
const streamBuf = 32

// Session is a multiplexed framed connection used by both client and tests.
type Session struct {
	conn     *FrameConn
	nextID   atomic.Uint64
	mu       sync.Mutex
	pending  map[string]*waiter
	closed   chan struct{}
	err      error
	onNotify func(Envelope)

	maxCallBytes    int
	maxSessionBytes int
	buffered        int
	highWater       int
	streams         int
}

func newSession(rw io.ReadWriteCloser, cfg HostConfig) *Session {
	s := &Session{
		conn:            NewFrameConn(rw),
		pending:         make(map[string]*waiter),
		closed:          make(chan struct{}),
		maxCallBytes:    cfg.maxCallBytes(),
		maxSessionBytes: cfg.maxSessionBytes(),
	}
	// The guest server arms its own FrameConn. Host sessions (Dial and
	// NewSession) did not, so a guest that stopped reading blocked Call
	// until the process restarted.
	s.conn.writeTimeout = cfg.writeTimeout()
	return s
}

// NewSession starts a reader loop on rw.
func NewSession(rw io.ReadWriteCloser) *Session {
	s := newSession(rw, HostConfig{})
	go s.readLoop()
	return s
}

// SetLimits replaces the byte caps. Zero keeps the current value. Call it
// before any stream is open; Dial's HostConfig is the production path.
func (s *Session) SetLimits(callBytes, sessionBytes int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if callBytes > 0 {
		s.maxCallBytes = callBytes
	}
	if sessionBytes > 0 {
		s.maxSessionBytes = sessionBytes
	}
	if s.maxSessionBytes < s.maxCallBytes {
		s.maxSessionBytes = s.maxCallBytes
	}
}

func (s *Session) callByteLimit() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxCallBytes
}

func (s *Session) bufferHighWater() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.highWater
}

// SetNotify receives guest→host frames that are not replies to a call.
// The callback runs on the read loop and must not block.
func (s *Session) SetNotify(fn func(Envelope)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.onNotify = fn
	s.mu.Unlock()
}

func (s *Session) readLoop() {
	defer close(s.closed)
	for {
		env, err := s.conn.Read()
		if err != nil {
			s.failPending(err)
			return
		}
		s.dispatch(env)
	}
}

func (s *Session) failPending(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
	for id, w := range s.pending {
		w.finished = true
		w.q.fail(id, err)
		delete(s.pending, id)
	}
	s.buffered = 0
	s.streams = 0
}

// dispatch delivers one envelope without blocking the read loop. Stream
// payload that would pass the call or session byte cap fails that call
// with a limit error. Other calls keep their frames.
func (s *Session) dispatch(env Envelope) {
	s.mu.Lock()
	w, ok := s.pending[env.ID]
	if !ok {
		fn := s.onNotify
		s.mu.Unlock()
		if fn != nil && env.Kind == KindNotify {
			fn(env)
		}
		return
	}
	n := streamBytes(env)
	if n > 0 {
		if w.queued+w.held+n > s.maxCallBytes {
			s.failLimitLocked(w, "call byte limit exceeded")
			s.mu.Unlock()
			return
		}
		if s.buffered+n > s.maxSessionBytes {
			s.failLimitLocked(w, "session byte limit exceeded")
			s.mu.Unlock()
			return
		}
		w.queued += n
		s.buffered += n
		if s.buffered > s.highWater {
			s.highWater = s.buffered
		}
	}
	terminal := env.Kind == KindResponse || env.Kind == KindError
	if terminal {
		delete(s.pending, w.id)
	}
	w.q.push(env)
	if terminal {
		w.q.close()
	}
	s.mu.Unlock()
}

// failLimitLocked queues a limit error and stops accepting frames for w.
// Bytes already queued stay until the caller reads them or the waiter is
// finished. The caller holds s.mu.
func (s *Session) failLimitLocked(w *waiter, msg string) {
	if w.finished {
		return
	}
	delete(s.pending, w.id)
	w.q.push(Envelope{
		ID: w.id, Kind: KindError,
		Error: &CallError{Code: ErrCodeLimit, Message: msg},
	})
	w.q.close()
}

func (s *Session) id() string {
	return fmt.Sprintf("%d", s.nextID.Add(1))
}

func (s *Session) register(id string, flow bool) (*waiter, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := &waiter{id: id, q: newFrameQueue(), flow: flow}
	s.pending[id] = w
	if flow {
		s.streams++
	}
	return w, nil
}

// finishWaiter drops the call and frees any payload the caller did not take.
func (s *Session) finishWaiter(w *waiter) {
	if w == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, w.id)
	if w.finished {
		return
	}
	w.finished = true
	s.buffered -= w.queued + w.held
	if s.buffered < 0 {
		s.buffered = 0
	}
	w.queued = 0
	w.held = 0
	if w.flow && s.streams > 0 {
		s.streams--
	}
	w.q.drop()
}

func (s *Session) pendingCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

func (s *Session) getErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		return io.EOF
	}
	return s.err
}

// Call sends a request and waits for the final response.
func (s *Session) Call(ctx context.Context, method string, payload any) (Envelope, error) {
	body, err := EncodePayload(payload)
	if err != nil {
		return Envelope{}, err
	}
	id := s.id()
	w, err := s.register(id, false)
	if err != nil {
		return Envelope{}, err
	}
	defer s.finishWaiter(w)

	if err := s.conn.WriteContext(ctx, Envelope{
		V: ProtocolVersion, ID: id, Kind: KindRequest, Method: method, Payload: body,
	}); err != nil {
		return Envelope{}, err
	}

	for {
		env, err := s.recvReleased(ctx, w)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return Envelope{}, err
			}
			select {
			case <-s.closed:
				return Envelope{}, s.getErr()
			default:
				return Envelope{}, err
			}
		}
		if env.Kind == KindError && env.Error != nil {
			return env, env.Error
		}
		if env.Kind == KindResponse {
			return env, nil
		}
		// stream frames ignored on Call; their bytes were released above
	}
}

// Stream sends a request and yields subsequent stream/response frames.
// The returned id lets the caller push upstream frames (stdin, resize)
// via WriteStream for the duration of the stream.
func (s *Session) Stream(ctx context.Context, method string, payload any) (<-chan Envelope, error) {
	_, ch, err := s.StreamWithID(ctx, method, payload)
	return ch, err
}

func (s *Session) StreamWithID(ctx context.Context, method string, payload any) (string, <-chan Envelope, error) {
	body, err := EncodePayload(payload)
	if err != nil {
		return "", nil, err
	}
	id := s.id()
	w, err := s.register(id, true)
	if err != nil {
		return "", nil, err
	}
	if err := s.conn.WriteContext(ctx, Envelope{
		V: ProtocolVersion, ID: id, Kind: KindRequest, Method: method, Payload: body,
		Window: s.callByteLimit(),
	}); err != nil {
		s.finishWaiter(w)
		return "", nil, err
	}
	// Unbuffered on purpose. A second queue here used to hold another
	// streamBuf frames after the session had already accepted them.
	out := make(chan Envelope)
	go func() {
		defer close(out)
		defer s.finishWaiter(w)
		defer func() {
			// Tell the agent to stop producing for this ID. Harmless if the
			// request already completed or the connection is gone. A fresh
			// deadline applies: ctx is already cancelled on this path, and
			// using it would skip the frame.
			_ = s.writeCancel(id, method)
		}()
		for {
			env, err := s.recvHeld(ctx, w)
			if err != nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-s.closed:
				return
			case out <- env:
				s.releaseHeld(w, env)
			}
			if env.Kind == KindResponse || env.Kind == KindError {
				return
			}
		}
	}()
	return id, out, nil
}

// recvReleased pops one frame and releases its byte charge. Call uses it
// because the caller is the consumer.
func (s *Session) recvReleased(ctx context.Context, w *waiter) (Envelope, error) {
	env, err := s.recvHeld(ctx, w)
	if err != nil {
		return Envelope{}, err
	}
	s.releaseHeld(w, env)
	return env, nil
}

// recvHeld pops one frame and moves its bytes from queued to held so a
// blocked handoff still counts against the cap.
func (s *Session) recvHeld(ctx context.Context, w *waiter) (Envelope, error) {
	env, err := w.q.pop(ctx, s.closed)
	if err != nil {
		return Envelope{}, err
	}
	n := streamBytes(env)
	if n == 0 {
		return env, nil
	}
	s.mu.Lock()
	if w.queued >= n {
		w.queued -= n
	} else {
		w.queued = 0
	}
	w.held += n
	s.mu.Unlock()
	return env, nil
}

func (s *Session) releaseHeld(w *waiter, env Envelope) {
	n := streamBytes(env)
	if n == 0 || w == nil {
		return
	}
	s.mu.Lock()
	if w.held >= n {
		w.held -= n
	} else {
		w.held = 0
	}
	s.buffered -= n
	if s.buffered < 0 {
		s.buffered = 0
	}
	flow := w.flow && !w.finished
	s.mu.Unlock()
	if flow {
		// Grant what the caller just took. The guest blocks instead of
		// filling the cap, so a slow reader keeps every byte. The grant
		// is written off the consumer so a peer that is not reading
		// cannot stall delivery or the limit error.
		s.noteGrant(w, n)
	}
}

// noteGrant coalesces credit and writes it from one goroutine. A stuck
// window write must not block the caller that just freed the bytes.
func (s *Session) noteGrant(w *waiter, n int) {
	if w == nil || n <= 0 {
		return
	}
	w.grantMu.Lock()
	w.grantPending += n
	if w.grantRunning {
		w.grantMu.Unlock()
		return
	}
	w.grantRunning = true
	w.grantMu.Unlock()
	go s.grantLoop(w)
}

func (s *Session) grantLoop(w *waiter) {
	for {
		w.grantMu.Lock()
		n := w.grantPending
		w.grantPending = 0
		if n == 0 {
			w.grantRunning = false
			w.grantMu.Unlock()
			return
		}
		id := w.id
		w.grantMu.Unlock()
		s.sendWindow(id, n)
	}
}

func (s *Session) sendWindow(id string, n int) {
	if n <= 0 {
		return
	}
	body, err := EncodePayload(WindowUpdate{Credit: n})
	if err != nil {
		return
	}
	timeout := s.conn.writeTimeout
	if timeout <= 0 {
		timeout = DefaultWriteTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = s.conn.WriteContext(ctx, Envelope{
		V: ProtocolVersion, ID: id, Kind: KindWindow, Payload: body,
	})
}

// WriteStream lets the caller push stdin (or similar) for an existing id.
// The write uses the session deadline. Callers that have a context use
// writeStream so a cancelled exec does not wait out that deadline.
func (s *Session) WriteStream(id string, payload any) error {
	timeout := s.conn.writeTimeout
	if timeout <= 0 {
		timeout = DefaultWriteTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.writeStream(ctx, id, payload)
}

func (s *Session) writeStream(ctx context.Context, id string, payload any) error {
	body, err := EncodePayload(payload)
	if err != nil {
		return err
	}
	return s.conn.WriteContext(ctx, Envelope{
		V: ProtocolVersion, ID: id, Kind: KindStream, Payload: body,
	})
}

func (s *Session) writeCancel(id, method string) error {
	timeout := s.conn.writeTimeout
	if timeout <= 0 {
		timeout = DefaultWriteTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.conn.WriteContext(ctx, Envelope{
		V: ProtocolVersion, ID: id, Kind: KindCancel, Method: method,
	})
}

// Close closes the underlying connection.
func (s *Session) Close() error {
	return s.conn.Close()
}

// Decode is a helper for tests and callers.
func Decode[T any](env Envelope) (T, error) {
	var zero T
	if len(env.Payload) == 0 {
		return zero, nil
	}
	err := json.Unmarshal(env.Payload, &zero)
	return zero, err
}
