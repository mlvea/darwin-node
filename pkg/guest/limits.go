package guest

import (
	"sync"
	"time"
)

const (
	DefaultMaxConns    = 8
	DefaultMaxExecs    = 4
	DefaultMaxInflight = 8

	MaxProbeTimeout = 60 * time.Second

	MaxLogLineBytes       = 256 << 10
	DefaultLogBufferBytes = 8 << 20

	MaxExecCapture = 1 << 20

	maxHandshakeNonces = 4096

	DefaultHandshakeTimeout = 10 * time.Second
	DefaultIdleTimeout      = 30 * time.Second
	DefaultWriteTimeout     = 30 * time.Second

	// DefaultMaxCallBytes is the unread stream payload one host call may
	// hold, and the most log bytes Client.Logs will collect. The guest log
	// ring is 8 MiB; past this the call fails instead of growing the heap.
	DefaultMaxCallBytes = 8 << 20
	// DefaultMaxSessionBytes is the unread stream payload one host session
	// may hold across every call. Eight full call buffers.
	DefaultMaxSessionBytes = 64 << 20

	// DefaultStdinWindow is the unread stdin payload a new host may have
	// in flight toward one exec. The guest grants those bytes back as the
	// process reads them. Zero in the handshake means an older guest that
	// does not grant credit.
	DefaultStdinWindow = 1 << 20
)

const (
	ErrCodeOverloaded      = "overloaded"
	ErrCodeUnauthorized    = "unauthorized"
	ErrCodeUnauthenticated = "unauthenticated"
	ErrCodeLimit           = "limit"
)

// HostConfig overrides host-side session limits. The zero value selects the
// defaults. Dial accepts one HostConfig; there is no process-wide flag.
type HostConfig struct {
	// WriteTimeout is the deadline for one host write. A guest that stops
	// reading fails the call here, or sooner when the call context expires.
	WriteTimeout time.Duration
	// MaxCallBytes caps unread stream payload for one call, and the log
	// bytes Client.Logs retains.
	MaxCallBytes int
	// MaxSessionBytes caps unread stream payload for the whole session.
	// A frame that would pass it fails that call.
	MaxSessionBytes int
	// HeartbeatInterval overrides how often the host sends Health.
	// Zero derives the interval from the guest's advertised idle timeout
	// (one third of it, strictly inside the window). Negative turns the
	// heartbeat off. There is no process flag.
	HeartbeatInterval time.Duration
}

func (c HostConfig) writeTimeout() time.Duration {
	if c.WriteTimeout > 0 {
		return c.WriteTimeout
	}
	return DefaultWriteTimeout
}

func (c HostConfig) maxCallBytes() int {
	if c.MaxCallBytes > 0 {
		return c.MaxCallBytes
	}
	return DefaultMaxCallBytes
}

func (c HostConfig) maxSessionBytes() int {
	n := c.MaxSessionBytes
	if n <= 0 {
		n = DefaultMaxSessionBytes
	}
	if call := c.maxCallBytes(); n < call {
		n = call
	}
	return n
}

// Limiter caps concurrent Serve connections and Exec calls.
type Limiter struct {
	conns *semaphore
	execs *semaphore
}

func NewLimiter(maxConns, maxExecs int) *Limiter {
	if maxConns <= 0 {
		maxConns = DefaultMaxConns
	}
	if maxExecs <= 0 {
		maxExecs = DefaultMaxExecs
	}
	return &Limiter{
		conns: newSemaphore(maxConns),
		execs: newSemaphore(maxExecs),
	}
}

func (l *Limiter) TryConn() bool {
	if l == nil {
		return true
	}
	return l.conns.TryAcquire()
}

func (l *Limiter) ReleaseConn() {
	if l == nil {
		return
	}
	l.conns.Release()
}

func (l *Limiter) TryExec() bool {
	if l == nil {
		return true
	}
	return l.execs.TryAcquire()
}

func (l *Limiter) ReleaseExec() {
	if l == nil {
		return
	}
	l.execs.Release()
}

type semaphore struct {
	ch chan struct{}
}

func newSemaphore(n int) *semaphore {
	if n <= 0 {
		n = 1
	}
	return &semaphore{ch: make(chan struct{}, n)}
}

func (s *semaphore) TryAcquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *semaphore) Release() {
	select {
	case <-s.ch:
	default:
	}
}

// handlerLive is shared across Handler copies so token reload, nonce
// anti-replay, and limiters apply to every Serve on the same agent.
type handlerLive struct {
	mu     sync.RWMutex
	token  string
	lim    *Limiter
	nonces *nonceSet
	// workload is set once the main process has been claimed, either by
	// Supervise or by the Workload RPC.
	workload bool
	// exits receives ReportExit for each live connection. pendingExit is
	// the last report, replayed onto a connection that comes up after it.
	// pendingSeq matches waitSeq when that report is the one a caller is
	// waiting to see written. exitWait is closed after that write, or
	// after it fails.
	exits       []exitSink
	pendingExit *ExitReport
	pendingSeq  uint64
	exitSeq     uint64
	exitWait    chan struct{}
	waitSeq     uint64
	// shut is closed by stopServing. Every Serve on this agent, including
	// its idle watchdog, returns. Close is once so a second Stop is safe.
	shut     chan struct{}
	shutOnce sync.Once
}

func newHandlerLive(token string, lim *Limiter) *handlerLive {
	if lim == nil {
		lim = NewLimiter(DefaultMaxConns, DefaultMaxExecs)
	}
	return &handlerLive{
		token:  token,
		lim:    lim,
		nonces: newNonceSet(maxHandshakeNonces),
		shut:   make(chan struct{}),
	}
}

// stopServing tells every Serve sharing this live state to return.
// A second call does nothing.
func (l *handlerLive) stopServing() {
	if l == nil || l.shut == nil {
		return
	}
	l.shutOnce.Do(func() { close(l.shut) })
}

func (l *handlerLive) getToken() string {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.token
}

func (l *handlerLive) setToken(token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.token = token
}

// claimWorkload lets one caller own the main process. A second claim is
// the host trying to apply a pod command after the image already started
// one, or a second Workload RPC.
func (l *handlerLive) claimWorkload() bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.workload {
		return false
	}
	l.workload = true
	return true
}

type nonceSet struct {
	mu    sync.Mutex
	max   int
	seen  map[string]struct{}
	order []string
}

func newNonceSet(max int) *nonceSet {
	if max <= 0 {
		max = maxHandshakeNonces
	}
	return &nonceSet{max: max, seen: make(map[string]struct{}, max)}
}

func (s *nonceSet) check(nonce string) *CallError {
	if nonce == "" {
		return &CallError{Code: ErrCodeUnauthorized, Message: "nonce required"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[nonce]; ok {
		return &CallError{Code: ErrCodeUnauthorized, Message: "replay"}
	}
	s.seen[nonce] = struct{}{}
	s.order = append(s.order, nonce)
	if len(s.order) > s.max {
		old := s.order[0]
		s.order = s.order[1:]
		delete(s.seen, old)
	}
	return nil
}

func overloadedEnvelope(env Envelope, msg string) Envelope {
	return Envelope{
		V:      ProtocolVersion,
		ID:     env.ID,
		Kind:   KindError,
		Method: env.Method,
		Error:  &CallError{Code: ErrCodeOverloaded, Message: msg},
	}
}
