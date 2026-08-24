package guest

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"
)

const (
	// ProtocolVersion is the envelope v field.
	ProtocolVersion = 1
	// MaxFrame is the largest accepted frame (16 MiB).
	MaxFrame = 16 << 20
)

// Kind is the envelope kind.
type Kind string

const (
	KindRequest  Kind = "req"
	KindResponse Kind = "res"
	KindStream   Kind = "stream"
	KindError    Kind = "err"
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
)

// Envelope is one framed JSON message.
type Envelope struct {
	V       int             `json:"v"`
	ID      string          `json:"id"`
	Kind    Kind            `json:"kind"`
	Method  string          `json:"method,omitempty"`
	Error   *CallError      `json:"error,omitempty"`
	Payload json.RawMessage `json:"payload,omitempty"`
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
	if len(body) > MaxFrame {
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
		return Envelope{}, fmt.Errorf("invalid frame length %d", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return Envelope{}, err
	}
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		return Envelope{}, err
	}
	if env.V != ProtocolVersion {
		return Envelope{}, fmt.Errorf("unsupported protocol version %d", env.V)
	}
	return env, nil
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
}

func NewFrameConn(rw io.ReadWriteCloser) *FrameConn {
	return &FrameConn{rw: rw}
}

func (c *FrameConn) Write(env Envelope) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	setWriteDeadline(c.rw, c.writeTimeout)
	err := WriteFrame(c.rw, env)
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		_ = c.rw.Close()
	}
	return err
}

func (c *FrameConn) Read() (Envelope, error) {
	return ReadFrame(c.rw)
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
	c, ok := rw.(interface{ SetWriteDeadline(time.Time) error })
	if !ok {
		return
	}
	if d <= 0 {
		_ = c.SetWriteDeadline(time.Time{})
		return
	}
	_ = c.SetWriteDeadline(time.Now().Add(d))
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
