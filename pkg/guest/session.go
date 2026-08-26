package guest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

const (
	callBuf   = 8
	streamBuf = 32
)

// Session is a multiplexed framed connection used by both client and tests.
type Session struct {
	conn    *FrameConn
	nextID  atomic.Uint64
	mu      sync.Mutex
	pending map[string]chan Envelope
	closed  chan struct{}
	err     error
}

// NewSession starts a reader loop on rw.
func NewSession(rw io.ReadWriteCloser) *Session {
	s := &Session{
		conn:    NewFrameConn(rw),
		pending: make(map[string]chan Envelope),
		closed:  make(chan struct{}),
	}
	go s.readLoop()
	return s
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
	for id, ch := range s.pending {
		select {
		case ch <- Envelope{ID: id, Kind: KindError, Error: &CallError{Code: "eof", Message: err.Error()}}:
		default:
		}
		close(ch)
	}
	s.pending = map[string]chan Envelope{}
}

// dispatch delivers one envelope without blocking readLoop. A full pending
// channel drops that RPC with "consumer too slow" so other calls still proceed.
func (s *Session) dispatch(env Envelope) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.pending[env.ID]
	if !ok {
		return
	}
	select {
	case ch <- env:
		if env.Kind == KindResponse || env.Kind == KindError {
			delete(s.pending, env.ID)
			close(ch)
		}
	default:
		delete(s.pending, env.ID)
		select {
		case ch <- Envelope{ID: env.ID, Kind: KindError, Error: &CallError{Code: "consumer_too_slow", Message: "consumer too slow"}}:
		default:
		}
		close(ch)
	}
}

func (s *Session) id() string {
	return fmt.Sprintf("%d", s.nextID.Add(1))
}

func (s *Session) register(id string, buf int) chan Envelope {
	ch := make(chan Envelope, buf)
	s.mu.Lock()
	s.pending[id] = ch
	s.mu.Unlock()
	return ch
}

// unregister drops the waiter without closing the channel. Only readLoop
// closes pending channels, to avoid send-on-closed races.
func (s *Session) unregister(id string) {
	s.mu.Lock()
	delete(s.pending, id)
	s.mu.Unlock()
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
	ch := s.register(id, callBuf)
	defer s.unregister(id)

	if err := s.conn.Write(Envelope{
		V: ProtocolVersion, ID: id, Kind: KindRequest, Method: method, Payload: body,
	}); err != nil {
		return Envelope{}, err
	}

	for {
		select {
		case <-ctx.Done():
			return Envelope{}, ctx.Err()
		case <-s.closed:
			return Envelope{}, s.getErr()
		case env, ok := <-ch:
			if !ok {
				return Envelope{}, io.EOF
			}
			if env.Kind == KindError && env.Error != nil {
				return env, env.Error
			}
			if env.Kind == KindResponse {
				return env, nil
			}
			// stream frames ignored on Call
		}
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
	ch := s.register(id, streamBuf)
	if err := s.conn.Write(Envelope{
		V: ProtocolVersion, ID: id, Kind: KindRequest, Method: method, Payload: body,
	}); err != nil {
		s.unregister(id)
		return "", nil, err
	}
	out := make(chan Envelope, streamBuf)
	go func() {
		defer close(out)
		defer s.unregister(id)
		defer func() {
			// Tell the agent to stop producing for this ID. Harmless if the
			// request already completed or the connection is gone.
			_ = s.conn.Write(Envelope{
				V: ProtocolVersion, ID: id, Kind: KindCancel, Method: method,
			})
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case <-s.closed:
				return
			case env, ok := <-ch:
				if !ok {
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-s.closed:
					return
				case out <- env:
				}
				if env.Kind == KindResponse || env.Kind == KindError {
					return
				}
			}
		}
	}()
	return id, out, nil
}

// WriteStream lets the caller push stdin (or similar) for an existing id.
func (s *Session) WriteStream(id string, payload any) error {
	body, err := EncodePayload(payload)
	if err != nil {
		return err
	}
	return s.conn.Write(Envelope{
		V: ProtocolVersion, ID: id, Kind: KindStream, Payload: body,
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
