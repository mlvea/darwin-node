package guest

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// ProtocolVersion is the envelope v field. MinProtocol and
	// MaxProtocol are the versions this binary speaks. They are equal
	// until a second version exists. A peer that omits the range is
	// treated as version 1.
	ProtocolVersion = 1
	MinProtocol     = 1
	MaxProtocol     = 1
	// MaxFrame is the largest accepted frame (16 MiB). Stream and
	// materialize frames may use it. Every other frame is capped at
	// MaxControlFrame. An over-limit frame closes the session.
	MaxFrame = 16 << 20
	// MaxControlFrame is the cap for handshake, health, probe, exit,
	// and the other non-stream RPCs.
	MaxControlFrame = 64 << 10
	// MaxJSONDepth is the deepest object or array nesting one frame
	// may contain. The envelope itself counts.
	MaxJSONDepth = 32
)

// errFrameLimit marks a frame the session must not continue after.
var errFrameLimit = errors.New("frame limit")

type frameLimitError struct{ msg string }

func (e *frameLimitError) Error() string { return e.msg }

func (e *frameLimitError) Unwrap() error { return errFrameLimit }

func allowsLargeFrame(env Envelope) bool {
	return env.Kind == KindStream || env.Method == MethodMaterialize
}

func maxFrameLen(env Envelope) int {
	if allowsLargeFrame(env) {
		return MaxFrame
	}
	return MaxControlFrame
}

// Kind is the envelope kind.
type Kind string

const (
	KindRequest  Kind = "req"
	KindResponse Kind = "res"
	KindStream   Kind = "stream"
	KindError    Kind = "err"
	// KindCancel is a one-way client-to-agent frame aborting the request
	// with the same ID (follows, long execs). Best-effort: unknown or
	// finished IDs are ignored.
	KindCancel Kind = "cancel"
	// KindNotify is a one-way guest-to-host frame. The host records it
	// and does not reply. Unknown IDs of any other kind are still dropped.
	KindNotify Kind = "notify"
	// KindWindow is a credit grant for one stream ID. Host→guest grants
	// let the guest write stdout. Guest→host grants let the host write
	// stdin. The payload is a WindowUpdate. A peer that ignores it can
	// still write; the sender then waits or fails at its own cap.
	KindWindow Kind = "window"
)

// Methods.
const (
	MethodHandshake   = "Handshake"
	MethodHealth      = "Health"
	MethodReady       = "Ready"
	MethodExec        = "Exec"
	MethodLogs        = "Logs"
	MethodProbe       = "Probe"
	MethodMetrics     = "Metrics"
	MethodNetInfo     = "NetInfo"
	MethodMaterialize = "Materialize"
	MethodShutdown    = "Shutdown"
	MethodSelftest    = "Selftest"
	// MethodWorkload starts the VM container's command. An older agent
	// replies unknown_method; the host then rejects the pod instead of
	// leaving the command unused.
	MethodWorkload = "Workload"
	// MethodAdopt is the fail-closed reseed barrier. It is additive on
	// protocol 1. An older agent omits the adopt capability and replies
	// unknown_method. The host rejects that pod unless an operator opt-in
	// says otherwise. See docs/guest-adopt.md.
	MethodAdopt = "Adopt"
	// MethodEphemeralKeychain installs the per-job keychain before the
	// workload. It is additive on protocol 1. An older agent omits the
	// capability and replies unknown_method. The host rejects that pod
	// unless an operator opt-in says otherwise. See docs/guest-keychain.md.
	MethodEphemeralKeychain = "EphemeralKeychain"
	// MethodExit is the guest→host exit notice. See ExitReport.
	MethodExit = "Exit"
)

// Envelope is one framed JSON message.
type Envelope struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	Kind    Kind            `json:"kind"`
	Method  string          `json:"method,omitempty"`
	Error   *CallError      `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
	// Window, on a host→guest request, is the initial stream credit in
	// payload bytes. Zero means the peer is not using flow control.
	Window int `json:"window,omitempty"`
}

// CallError is a protocol-level error.
type CallError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *CallError) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// WriteFrame writes one length-prefixed JSON envelope.
func WriteFrame(w io.Writer, env Envelope) error {
	if env.V == 0 {
		env.V = ProtocolVersion
	}
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	if len(body) > maxFrameLen(env) {
		return fmt.Errorf("frame too large: %d", len(body))
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}

// ReadFrame reads one length-prefixed JSON envelope.
func ReadFrame(r io.Reader) (Envelope, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return Envelope{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > MaxFrame {
		return Envelope{}, &frameLimitError{msg: fmt.Sprintf("invalid frame length %d", n)}
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Envelope{}, err
	}
	if jsonTooDeep(body, MaxJSONDepth) {
		return Envelope{}, &frameLimitError{msg: "json nesting too deep"}
	}
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Envelope{}, err
	}
	if len(body) > maxFrameLen(env) {
		return Envelope{}, &frameLimitError{msg: fmt.Sprintf("frame too large: %d", len(body))}
	}
	if env.V != ProtocolVersion {
		return Envelope{}, fmt.Errorf("unsupported protocol version %d", env.V)
	}
	return env, nil
}

// jsonTooDeep reports whether b nests objects or arrays deeper than max.
// Bytes inside strings are not structure. The scan is a bound, not a
// full parse: a hostile document is rejected before Unmarshal walks it.
func jsonTooDeep(b []byte, max int) bool {
	if max <= 0 {
		max = MaxJSONDepth
	}
	depth := 0
	inStr := false
	esc := false
	for _, c := range b {
		if inStr {
			if esc {
				esc = false
				continue
			}
			switch c {
			case '\\':
				esc = true
			case '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
			if depth > max {
				return true
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return false
}

// EncodePayload marshals v to raw JSON.
func EncodePayload(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// DecodePayload unmarshals env.Payload into v.
func DecodePayload[T any](env Envelope) (T, error) {
	var out T
	if len(env.Payload) == 0 {
		return out, nil
	}
	err := json.Unmarshal(env.Payload, &out)
	return out, err
}

// FrameConn serializes writes to a stream.
type FrameConn struct {
	rw           io.ReadWriteCloser
	mu           sync.Mutex
	writeTimeout time.Duration
	// wake bumps when a call context gives up. The writer holding mu
	// re-arms its deadline so a later, shorter call is not stuck behind
	// a guest that has stopped reading.
	wake atomic.Uint64
}

func NewFrameConn(rw io.ReadWriteCloser) *FrameConn {
	return &FrameConn{rw: rw}
}

func (c *FrameConn) Write(env Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	setWriteDeadline(c.rw, c.writeTimeout)
	err := WriteFrame(c.rw, env)
	if isNetTimeout(err) {
		_ = c.rw.Close()
	}
	return err
}

// WriteContext writes one frame. The deadline is the sooner of the
// connection write timeout and ctx. A cancelled ctx aborts this write and
// any write already blocked on the same connection, then closes it: a
// partial frame cannot be resynchronized. The connection stays open when
// ctx is already done before any byte is written.
func (c *FrameConn) WriteContext(ctx context.Context, env Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		c.wake.Add(1)
		expireWriteDeadline(c.rw)
		close(done)
	})

	c.mu.Lock()
	err := c.writeLocked(ctx, env)
	// Stop (or wait out) the canceller before clearing the deadline, still
	// holding mu, so the clear cannot race the next writer's arm.
	if !stop() {
		<-done
	}
	setWriteDeadlineAt(c.rw, time.Time{})
	c.mu.Unlock()
	return err
}

func (c *FrameConn) writeLocked(ctx context.Context, env Envelope) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timeout := c.writeTimeout
	if timeout <= 0 {
		timeout = DefaultWriteTimeout
	}
	deadline := time.Now().Add(timeout)
	// A net timeout and ctx.Err can surface in either order when the
	// armed deadline is the context's. Prefer the context in that case
	// so a short pod-delete grace is what the caller observes.
	armedCtx := false
	if d, ok := ctx.Deadline(); ok && !d.After(deadline) {
		deadline = d
		armedCtx = true
	}
	mark := c.wake.Load()
	setWriteDeadlineAt(c.rw, deadline)
	if ctx.Err() != nil || c.wake.Load() != mark {
		expireWriteDeadline(c.rw)
	}
	err := WriteFrame(c.rw, env)
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		_ = c.rw.Close()
		return ctx.Err()
	}
	if isNetTimeout(err) {
		_ = c.rw.Close()
		if armedCtx {
			return context.DeadlineExceeded
		}
		return fmt.Errorf("guest write deadline exceeded: %w", err)
	}
	return err
}

func isNetTimeout(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (c *FrameConn) Read() (Envelope, error) {
	env, err := ReadFrame(c.rw)
	if errors.Is(err, errFrameLimit) {
		_ = c.rw.Close()
	}
	return env, err
}

func setReadDeadline(rw io.ReadWriteCloser, d time.Duration) {
	c, ok := rw.(interface{ SetReadDeadline(time.Time) error })
	if !ok {
		return
	}
	if d <= 0 {
		_ = c.SetReadDeadline(time.Time{})
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(d))
}

func setWriteDeadline(rw io.ReadWriteCloser, d time.Duration) {
	if d <= 0 {
		setWriteDeadlineAt(rw, time.Time{})
		return
	}
	setWriteDeadlineAt(rw, time.Now().Add(d))
}

func setWriteDeadlineAt(rw io.ReadWriteCloser, t time.Time) {
	c, ok := rw.(interface{ SetWriteDeadline(time.Time) error })
	if !ok {
		return
	}
	_ = c.SetWriteDeadline(t)
}

// expireWriteDeadline unblocks a write parked in the peer window. A stream
// with no deadline support is closed instead, which is the only way to
// interrupt it.
func expireWriteDeadline(rw io.ReadWriteCloser) {
	c, ok := rw.(interface{ SetWriteDeadline(time.Time) error })
	if !ok {
		_ = rw.Close()
		return
	}
	_ = c.SetWriteDeadline(time.Now())
}

func setConnDeadline(rw io.ReadWriteCloser, d time.Duration) {
	c, ok := rw.(interface{ SetDeadline(time.Time) error })
	if !ok {
		setReadDeadline(rw, d)
		setWriteDeadline(rw, d)
		return
	}
	if d <= 0 {
		_ = c.SetDeadline(time.Time{})
		return
	}
	_ = c.SetDeadline(time.Now().Add(d))
}

func (c *FrameConn) Close() error {
	return c.rw.Close()
}
