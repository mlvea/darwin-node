package guest

import (
	"context"
	"io"
	"sync"
)

// waiter is one in-flight call. queued and held are stream payload bytes
// not yet delivered to the caller. The read loop never blocks on a waiter:
// a frame that does not fit fails the call.
type waiter struct {
	id       string
	q        *frameQueue
	flow     bool
	queued   int
	held     int
	finished bool

	// grantMu covers the coalesced credit the host still owes the guest.
	// The grant goroutine writes it; the stream consumer must not.
	grantMu      sync.Mutex
	grantPending int
	grantRunning bool
}

// frameQueue is an unbounded-count, explicitly closed list of envelopes.
// Byte caps live on the session so one queue cannot answer for its siblings.
type frameQueue struct {
	mu     sync.Mutex
	frames []Envelope
	closed bool
	notify chan struct{}
}

func newFrameQueue() *frameQueue {
	return &frameQueue{notify: make(chan struct{}, 1)}
}

func (q *frameQueue) signal() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *frameQueue) push(env Envelope) {
	q.mu.Lock()
	q.frames = append(q.frames, env)
	q.mu.Unlock()
	q.signal()
}

func (q *frameQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *frameQueue) fail(id string, err error) {
	q.mu.Lock()
	if !q.closed {
		msg := ""
		if err != nil {
			msg = err.Error()
		}
		q.frames = append(q.frames, Envelope{
			ID: id, Kind: KindError,
			Error: &CallError{Code: "eof", Message: msg},
		})
		q.closed = true
	}
	q.mu.Unlock()
	q.signal()
}

func (q *frameQueue) drop() {
	q.mu.Lock()
	for i := range q.frames {
		q.frames[i] = Envelope{}
	}
	q.frames = nil
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *frameQueue) pop(ctx context.Context, sessClosed <-chan struct{}) (Envelope, error) {
	for {
		q.mu.Lock()
		if len(q.frames) > 0 {
			env := q.frames[0]
			q.frames[0] = Envelope{}
			q.frames = q.frames[1:]
			if len(q.frames) == 0 {
				q.frames = nil
			}
			q.mu.Unlock()
			return env, nil
		}
		if q.closed {
			q.mu.Unlock()
			return Envelope{}, io.EOF
		}
		q.mu.Unlock()
		select {
		case <-ctx.Done():
			return Envelope{}, ctx.Err()
		case <-sessClosed:
			q.mu.Lock()
			empty := len(q.frames) == 0
			q.mu.Unlock()
			if empty {
				return Envelope{}, io.EOF
			}
		case <-q.notify:
		}
	}
}

func streamBytes(env Envelope) int {
	if env.Kind != KindStream {
		return 0
	}
	return len(env.Payload)
}
