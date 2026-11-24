package guest

import (
	"encoding/json"
	"io"
	"sync"
)

// upstreamRouter delivers client→agent stream frames to the exec that owns
// the envelope ID. Frames are queued until that exec reads them. A full
// queue does not drop a frame and does not invent an EOF: the host stops
// sending when its stdin credit is spent, and the guest grants credit back
// as the process reads.
type upstreamRouter struct {
	mu      sync.Mutex
	streams map[string]*stdinQueue
}

type stdinQueue struct {
	mu     sync.Mutex
	frames []Envelope
	closed bool
	notify chan struct{}
}

func newStdinQueue() *stdinQueue {
	return &stdinQueue{notify: make(chan struct{}, 1)}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func newUpstreamRouter() *upstreamRouter {
	return &upstreamRouter{streams: map[string]*stdinQueue{}}
}

func (u *upstreamRouter) register(id string) *stdinQueue {
	q := newStdinQueue()
	u.mu.Lock()
	if old := u.streams[id]; old != nil {
		old.close()
	}
	u.streams[id] = q
	u.mu.Unlock()
	return q
}

func (u *upstreamRouter) get(id string) *stdinQueue {
	if u == nil {
		return nil
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.streams[id]
}

func (u *upstreamRouter) unregister(id string) {
	if u == nil {
		return
	}
	u.mu.Lock()
	q := u.streams[id]
	delete(u.streams, id)
	u.mu.Unlock()
	if q != nil {
		q.close()
	}
}

func (u *upstreamRouter) forward(env Envelope) {
	u.mu.Lock()
	q := u.streams[env.ID]
	u.mu.Unlock()
	if q == nil {
		return
	}
	q.push(env)
}

func (q *stdinQueue) signal() {
	select {
	case q.notify <- struct{}{}:
	default:
	}
}

func (q *stdinQueue) push(env Envelope) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.frames = append(q.frames, env)
	q.mu.Unlock()
	q.signal()
}

func (q *stdinQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

func (q *stdinQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.frames)
}

func (q *stdinQueue) pop(abort <-chan struct{}) (Envelope, bool) {
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
			return env, true
		}
		closed := q.closed
		q.mu.Unlock()
		if closed {
			return Envelope{}, false
		}
		select {
		case <-q.notify:
		case <-abort:
			return Envelope{}, false
		}
	}
}

// windowSender coalesces guest→host stdin credit so a process Read does
// not block on the connection.
type windowSender struct {
	mu      sync.Mutex
	pending int
	running bool
	fc      *FrameConn
	id      string
}

func newWindowSender(fc *FrameConn, id string) *windowSender {
	if fc == nil || id == "" {
		return nil
	}
	return &windowSender{fc: fc, id: id}
}

func (w *windowSender) add(n int) {
	if w == nil || n <= 0 {
		return
	}
	w.mu.Lock()
	w.pending += n
	if w.running {
		w.mu.Unlock()
		return
	}
	w.running = true
	w.mu.Unlock()
	go w.loop()
}

func (w *windowSender) loop() {
	for {
		w.mu.Lock()
		n := w.pending
		w.pending = 0
		if n == 0 {
			w.running = false
			w.mu.Unlock()
			return
		}
		id := w.id
		w.mu.Unlock()
		body, err := json.Marshal(WindowUpdate{Credit: n})
		if err != nil {
			continue
		}
		_ = w.fc.Write(Envelope{V: ProtocolVersion, ID: id, Kind: KindWindow, Payload: body})
	}
}

// execStdinReader adapts upstream ExecStdin and TtyResize frames into an
// io.Reader. Resize frames are surfaced on resize and never enter the byte
// stream. Abort wakes a blocked Read once the command is done, so a process
// that exits without reading does not hold the exec slot. Consumed payload
// bytes are reported through onGrant so the host can send the next frame.
type execStdinReader struct {
	q         *stdinQueue
	resize    chan<- TtyResize
	abort     chan struct{}
	abortOnce sync.Once
	current   []byte
	grantLeft int
	eof       bool
	onGrant   func(int)
}

func newExecStdinReader(q *stdinQueue, interactive bool) *execStdinReader {
	return &execStdinReader{q: q, eof: !interactive || q == nil, abort: make(chan struct{})}
}

func (r *execStdinReader) abortNow() { r.abortOnce.Do(func() { close(r.abort) }) }

func (r *execStdinReader) grant(n int) {
	if n > 0 && r.onGrant != nil {
		r.onGrant(n)
	}
}

func (r *execStdinReader) pull() bool {
	if r.q == nil {
		r.eof = true
		return false
	}
	for {
		env, ok := r.q.pop(r.abort)
		if !ok {
			r.eof = true
			return false
		}
		if ev, err := DecodePayload[TtyResize](env); err == nil && (ev.Cols > 0 || ev.Rows > 0) {
			if r.resize != nil {
				select {
				case r.resize <- ev:
				default:
				}
			}
			continue
		}
		ev, err := DecodePayload[ExecStdin](env)
		if err != nil {
			continue
		}
		if len(ev.Data) > 0 {
			r.current = append([]byte(nil), ev.Data...)
		}
		r.grantLeft = len(env.Payload)
		if ev.EOF {
			r.eof = true
		}
		if len(r.current) == 0 {
			r.grant(r.grantLeft)
			r.grantLeft = 0
			if r.eof {
				return false
			}
			continue
		}
		return true
	}
}

func (r *execStdinReader) Read(p []byte) (int, error) {
	for len(r.current) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		if !r.pull() {
			return 0, io.EOF
		}
	}
	n := copy(p, r.current)
	r.current = r.current[n:]
	if len(r.current) == 0 && r.grantLeft > 0 {
		r.grant(r.grantLeft)
		r.grantLeft = 0
	}
	return n, nil
}
