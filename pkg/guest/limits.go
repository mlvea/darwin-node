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
)

const (
	ErrCodeOverloaded      = "overloaded"
	ErrCodeUnauthorized    = "unauthorized"
	ErrCodeUnauthenticated = "unauthenticated"
)

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
}

func newHandlerLive(token string, lim *Limiter) *handlerLive {
	if lim == nil {
		lim = NewLimiter(DefaultMaxConns, DefaultMaxExecs)
	}
	return &handlerLive{
		token:  token,
		lim:    lim,
		nonces: newNonceSet(maxHandshakeNonces),
	}
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
