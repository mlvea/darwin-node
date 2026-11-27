package guest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Handler is the in-guest implementation of the agent RPC.
type Handler struct {
	Token        string
	AgentVersion string
	LogBuffer    *LogBuffer
	Started      time.Time

	// AllowInsecureNoToken is dev-only. Empty Token otherwise rejects every handshake.
	AllowInsecureNoToken bool

	// Limiter bounds connections and execs. Nil uses DefaultMaxConns/DefaultMaxExecs.
	Limiter *Limiter

	// HandshakeTimeout is the deadline for the first authenticated frame (default 10s).
	HandshakeTimeout time.Duration
	// IdleTimeout is reset on every successful frame read after handshake (default 30s).
	IdleTimeout time.Duration
	// clock drives IdleTimeout. Nil uses the wall clock. Tests set a manual
	// clock so the window can elapse without sleeping. Not part of the
	// agent protocol.
	clock idleClock
	// WriteTimeout is applied per frame write (default 30s).
	WriteTimeout time.Duration

	live *handlerLive

	// Hooks allow tests to stub OS work. Nil → real implementation.
	ExecFn        func(ctx context.Context, req ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	ProbeHTTPFn   func(ctx context.Context, req ProbeReq) ProbeRes
	NetInfoFn     func() (NetInfoRes, error)
	MetricsFn     func() (MetricsRes, error)
	MaterializeFn func(req MaterializeReq) MaterializeRes
	// WorkloadFn, when set, handles the Workload RPC instead of starting a
	// process. The fake runtime and tests use it.
	WorkloadFn func(ctx context.Context, req WorkloadReq) error
	// OnWorkload observes a Workload RPC and then lets WorkloadFn or the
	// real process start. Nil changes nothing. SimVZ counts calls here so
	// a workload still exits and reports that exit.
	OnWorkload func(ctx context.Context, req WorkloadReq)
	// OnKeychain observes an EphemeralKeychain RPC and then lets the
	// platform install the job keychain. Nil changes nothing. SimVZ counts
	// calls here.
	OnKeychain func(ctx context.Context, req EphemeralKeychainReq)
	// KeychainPlatform is the OS the job keychain touches. Nil uses an
	// in-memory image keychain. Linux, SimVZ, and the fake runtime leave
	// it nil or set that in-memory platform. The darwin guest agent sets
	// the real one.
	KeychainPlatform KeychainPlatform
	// AdoptFn, when set, handles Adopt instead of AdoptPlatform. SimVZ
	// uses it to inject a bad reseed. Nil uses AdoptPlatform, or the
	// in-memory platform when that is also nil.
	AdoptFn          func(ctx context.Context, req AdoptReq) (AdoptRes, error)
	AdoptPlatform    AdoptPlatform
	ShutdownFn       func(ctx context.Context, req ShutdownReq) error
	SelftestFn       func(req SelftestReq) SelftestRes
	MetalAvailableFn func() bool
}

// ReportExit sends rep on every agent connection this handler is serving.
// A report with no exit code is ignored. The guest calls this before it
// halts; the host does not ask. Safe to call before a connection exists:
// the last report is replayed when one is accepted.
func (h *Handler) ReportExit(rep ExitReport) {
	if h == nil || !rep.Reported() {
		return
	}
	h.Init()
	h.live.publishExit(rep)
}

// Init allocates shared live state (token, nonces, limiter). Call once
// before copying Handler into concurrent Serve goroutines.
func (h *Handler) Init() {
	if h.live != nil {
		return
	}
	h.live = newHandlerLive(h.Token, h.Limiter)
}

// SetToken updates the handshake token visible to in-flight Serve loops.
func (h *Handler) SetToken(token string) {
	h.Token = token
	h.Reload()
}

// Close stops every Serve that shares this handler's live state. The
// idle watchdog returns and closes its connection, so a host that still
// holds the client cannot keep the goroutine parked until IdleTimeout.
// A second call is a no-op. Token reload does not call Close.
func (h *Handler) Close() {
	if h == nil || h.live == nil {
		return
	}
	h.live.stopServing()
}

// Reload copies Handler.Token into the shared live token.
func (h *Handler) Reload() {
	h.Init()
	h.live.setToken(h.Token)
}

// CurrentToken is the token Serve will accept (live, else Token).
func (h Handler) handshakeTimeout() time.Duration {
	if h.HandshakeTimeout > 0 {
		return h.HandshakeTimeout
	}
	return DefaultHandshakeTimeout
}

func (h Handler) idleTimeout() time.Duration {
	if h.IdleTimeout > 0 {
		return h.IdleTimeout
	}
	return DefaultIdleTimeout
}

func (h Handler) writeTimeout() time.Duration {
	if h.WriteTimeout > 0 {
		return h.WriteTimeout
	}
	return DefaultWriteTimeout
}

func (h *Handler) CurrentToken() string {
	if h != nil && h.live != nil {
		return h.live.getToken()
	}
	if h == nil {
		return ""
	}
	return h.Token
}

// ReadTokenFile returns the trimmed contents of a guest agent token file.
func ReadTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// WatchTokenFile re-reads path until ctx is done. Empty or missing files
// are ignored so a late virtio-fs mount can heal without clearing a token.
func (h *Handler) WatchTokenFile(ctx context.Context, path string, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Second
	}
	load := func() {
		tok, err := ReadTokenFile(path)
		if err != nil || tok == "" {
			return
		}
		h.SetToken(tok)
	}
	load()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			load()
		}
	}
}

// Serve reads requests from rw until it closes or ctx is done.
func Serve(ctx context.Context, rw io.ReadWriteCloser, h Handler) error {
	if h.Started.IsZero() {
		h.Started = time.Now()
	}
	if h.LogBuffer == nil {
		h.LogBuffer = NewLogBuffer(256)
	}
	if h.live == nil {
		h.live = newHandlerLive(h.Token, h.Limiter)
	}
	fc := NewFrameConn(rw)
	fc.writeTimeout = h.writeTimeout()

	var wg sync.WaitGroup
	defer wg.Wait()

	exitStop := func() {}
	var exitOnce sync.Once
	writerDone := make(chan struct{})
	startExitWriter := func() {
		exitOnce.Do(func() {
			ch := make(chan exitNote, 1)
			// Closing the connection unblocks Write when the host is gone.
			exitStop = h.live.addExitSink(ch, func() { _ = rw.Close() })
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					select {
					case <-ctx.Done():
						return
					case <-writerDone:
						return
					case note := <-ch:
						env, err := exitNotifyEnvelope(note.rep)
						var werr error
						if err == nil {
							werr = fc.Write(env)
						}
						h.live.finishExitWait(note.seq)
						if err != nil || werr != nil {
							return
						}
					}
				}
			}()
		})
	}
	// Runs before Wait: drop the sink and wake the writer, then join it.
	// A Serve that never handshakes still closes writerDone.
	defer func() {
		exitStop()
		close(writerDone)
	}()

	if !h.live.lim.TryConn() {
		env, err := fc.Read()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		return fc.Write(overloadedEnvelope(env, "too many connections"))
	}
	defer h.live.lim.ReleaseConn()

	var handshook atomic.Bool
	defer func() { _ = rw.Close() }()

	inflight := newSemaphore(DefaultMaxInflight)
	conn := &connState{upstream: newUpstreamRouter(), fc: fc}
	// A handler blocked on stream credit must wake when the read loop
	// ends. Otherwise Serve waits forever on that handler.
	defer conn.cancelAll()

	setConnDeadline(rw, h.handshakeTimeout())

	// Idle deadline instead of per-read deadlines: arming a deadline before
	// every read raced stream accounting (an open shell is silent by
	// design). The connection closes only when no frame has arrived for the
	// idle window AND no interactive stream is running. The watchdog
	// goroutine only waits for shutdown; the deadline is a timer, so the
	// goroutine does not poll.
	track := newIdleTrack(h, rw, conn)
	wdone := make(chan struct{})
	// Join the watchdog before Serve returns. close(wdone) is deferred
	// ahead of wg.Wait, so the goroutine observes shutdown and Done
	// before Wait unblocks. stop runs after that signal.
	defer track.stop()
	defer close(wdone)
	wg.Add(1)
	go func() {
		defer wg.Done()
		h.idleWatchdog(ctx, rw, wdone)
	}()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		env, err := fc.Read()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		// Any accepted frame is client activity. Reset the idle deadline
		// from this read. The old watchdog kept the connect-time stamp, so
		// a busy non-interactive session (probes, health, logs) was closed
		// one IdleTimeout after dial.
		track.touch()
		// Client→agent stream frames (stdin, tty resize) route to the
		// goroutine serving that envelope ID. Unknown IDs are dropped.
		if env.Kind == KindStream {
			conn.upstream.forward(env)
			continue
		}
		// One-way cancellation for long-running streams (follows, execs).
		if env.Kind == KindCancel {
			conn.cancelByID(env.ID)
			continue
		}
		// Host→guest stream credit. Unknown IDs are ignored so a grant
		// for a finished call cannot stall the read loop.
		if env.Kind == KindWindow {
			var up WindowUpdate
			if err := json.Unmarshal(env.Payload, &up); err == nil {
				conn.addCredit(env.ID, up.Credit)
			}
			continue
		}
		if env.Kind != KindRequest {
			continue
		}

		// Handshake stays serial so later methods cannot race the token check.
		if env.Method == MethodHandshake || !handshook.Load() {
			if err := h.writeDispatch(ctx, fc, &handshook, env, conn, false); err != nil {
				return err
			}
			if handshook.Load() {
				setConnDeadline(rw, 0)
				startExitWriter()
			}
			continue
		}

		if !inflight.TryAcquire() {
			_ = fc.Write(overloadedEnvelope(env, "too many in-flight requests"))
			continue
		}
		// Increment BEFORE spawning so the deadline check at the top of this
		// loop can never observe stale zero during scheduling delay.
		interactiveReq := false
		if env.Method == MethodExec {
			if req, derr := DecodePayload[ExecReq](env); derr == nil {
				interactiveReq = req.Stdin || req.TTY
			}
		}
		if interactiveReq {
			conn.liveInteractive.Add(1)
			// Register before the next read. Stdin frames follow this
			// request on the same connection, and a queue created inside
			// the handler goroutine would miss them.
			conn.upstream.register(env.ID)
		}
		wg.Add(1)
		go func(env Envelope, interactive bool) {
			defer wg.Done()
			defer inflight.Release()
			_ = h.writeDispatch(ctx, fc, &handshook, env, conn, interactive)
		}(env, interactiveReq)
	}
}

// connState carries per-connection state into dispatch goroutines.
type connState struct {
	upstream        *upstreamRouter
	fc              *FrameConn
	liveInteractive atomic.Int32

	mu      sync.Mutex
	cancels map[string]context.CancelFunc
	credit  map[string]*streamCredit
}

func (c *connState) registerCancel(id string, cancel context.CancelFunc) {
	c.mu.Lock()
	if c.cancels == nil {
		c.cancels = map[string]context.CancelFunc{}
	}
	c.cancels[id] = cancel
	c.mu.Unlock()
}

// streamCredit is the guest's view of one stream's send window.
type streamCredit struct {
	mu     sync.Mutex
	left   int
	window int
	notify chan struct{}
}

func (c *connState) armCredit(id string, window int) {
	if c == nil || window <= 0 || id == "" {
		return
	}
	cr := &streamCredit{left: window, window: window, notify: make(chan struct{}, 1)}
	c.mu.Lock()
	if c.credit == nil {
		c.credit = map[string]*streamCredit{}
	}
	c.credit[id] = cr
	c.mu.Unlock()
}

func (c *connState) disarmCredit(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	delete(c.credit, id)
	c.mu.Unlock()
}

func (c *connState) addCredit(id string, n int) {
	if c == nil || n <= 0 {
		return
	}
	c.mu.Lock()
	cr := c.credit[id]
	c.mu.Unlock()
	if cr == nil {
		return
	}
	cr.mu.Lock()
	cr.left += n
	cr.mu.Unlock()
	select {
	case cr.notify <- struct{}{}:
	default:
	}
}

// acquireCredit waits until the host has granted n payload bytes. A frame
// larger than the whole window is sent anyway; the host rejects it at the
// cap instead of deadlocking the producer.
func (c *connState) acquireCredit(ctx context.Context, id string, n int) error {
	if c == nil || n <= 0 {
		return nil
	}
	c.mu.Lock()
	cr := c.credit[id]
	c.mu.Unlock()
	if cr == nil || (cr.window > 0 && n > cr.window) {
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		cr.mu.Lock()
		if cr.left >= n {
			cr.left -= n
			cr.mu.Unlock()
			return nil
		}
		cr.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-cr.notify:
		}
	}
}

func (c *connState) cancelByID(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	cancel, ok := c.cancels[id]
	delete(c.cancels, id)
	c.mu.Unlock()
	if ok {
		cancel()
	}
}

func (c *connState) cancelAll() {
	if c == nil {
		return
	}
	c.mu.Lock()
	cancels := c.cancels
	c.cancels = nil
	c.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

func (h *Handler) writeDispatch(ctx context.Context, fc *FrameConn, handshook *atomic.Bool, env Envelope, conn *connState, interactive bool) error {
	// The read loop increments liveInteractive before this runs so the idle
	// watchdog cannot close the socket during scheduling. Every return path,
	// including overloaded exec, must decrement or the connection never idles.
	if interactive && conn != nil {
		defer conn.liveInteractive.Add(-1)
	}
	// Every request gets a derived context that dies with the write side:
	// if the client vanishes mid-stream, producers blocked on channel sends
	// are released instead of leaking for the life of the connection.
	sctx, cancel := context.WithCancel(ctx)
	if conn != nil {
		conn.registerCancel(env.ID, cancel)
		defer conn.cancelByID(env.ID)
	} else {
		defer cancel()
	}
	res, stream := h.dispatch(sctx, handshook, env, conn)
	if stream != nil {
		// Unregister only after the stream has been drained. Doing it from a
		// second reader on the same channel stole stdout/stderr frames.
		if interactive && conn != nil {
			defer conn.upstream.unregister(env.ID)
		}
		if conn != nil && env.Window > 0 {
			conn.armCredit(env.ID, env.Window)
			defer conn.disarmCredit(env.ID)
		}
		for ev := range stream {
			if ev.Kind == KindStream && conn != nil && env.Window > 0 {
				if err := conn.acquireCredit(sctx, env.ID, len(ev.Payload)); err != nil {
					return err
				}
			}
			if err := fc.Write(ev); err != nil {
				return err
			}
		}
		return nil
	}
	if interactive && conn != nil {
		conn.upstream.unregister(env.ID)
	}
	return fc.Write(res)
}

// sendFrame delivers one envelope unless ctx is done. Producers must not
// block forever when the consumer has stopped reading.
func sendFrame(ctx context.Context, ch chan<- Envelope, ev Envelope) bool {
	select {
	case <-ctx.Done():
		return false
	case ch <- ev:
		return true
	}
}

func (h *Handler) dispatch(ctx context.Context, handshook *atomic.Bool, env Envelope, conn *connState) (Envelope, <-chan Envelope) {
	reply := func(payload any, err *CallError) Envelope {
		out := Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: env.Method}
		if err != nil {
			out.Kind = KindError
			out.Error = err
			return out
		}
		b, mErr := EncodePayload(payload)
		if mErr != nil {
			out.Kind = KindError
			out.Error = &CallError{Code: "encode", Message: mErr.Error()}
			return out
		}
		out.Payload = b
		return out
	}

	if env.Method != MethodHandshake && !handshook.Load() {
		return reply(nil, &CallError{Code: ErrCodeUnauthenticated, Message: "handshake required"}), nil
	}

	switch env.Method {
	case MethodHandshake:
		req, err := DecodePayload[HandshakeReq](env)
		if err != nil {
			return reply(nil, &CallError{Code: "bad_request", Message: err.Error()}), nil
		}
		if err := authorizeHandshake(*h, req); err != nil {
			return reply(nil, err), nil
		}
		if err := h.checkNonce(req.Nonce); err != nil {
			return reply(nil, err), nil
		}
		chosen, perr := selectProtocol(req.MinProtocol, req.MaxProtocol)
		if perr != nil {
			return reply(nil, &CallError{Code: "protocol", Message: perr.Error()}), nil
		}
		handshook.Store(true)
		host, _ := os.Hostname()
		metal := false
		if h.MetalAvailableFn != nil {
			metal = h.MetalAvailableFn()
		} else {
			metal = defaultMetalAvailable()
		}
		return reply(HandshakeRes{
			OK:             true,
			AgentVersion:   h.AgentVersion,
			Hostname:       host,
			OSVersion:      runtime.GOOS + "/" + runtime.GOARCH,
			Protocol:       chosen,
			MetalAvailable: metal,
			IdleMS:         int(h.idleTimeout() / time.Millisecond),
			StdinWindow:    DefaultStdinWindow,
			Capabilities:   GuestCapabilities(),
		}, nil), nil

	case MethodHealth:
		return reply(HealthRes{OK: true, Uptime: time.Since(h.Started).String()}, nil), nil

	case MethodReady:
		return reply(ReadyRes{Ready: true, Message: "agent running"}, nil), nil

	case MethodNetInfo:
		fn := h.NetInfoFn
		if fn == nil {
			fn = defaultNetInfo
		}
		info, err := fn()
		if err != nil {
			return reply(nil, &CallError{Code: "netinfo", Message: err.Error()}), nil
		}
		return reply(info, nil), nil

	case MethodMetrics:
		fn := h.MetricsFn
		if fn == nil {
			fn = defaultMetrics
		}
		m, err := fn()
		if err != nil {
			return reply(nil, &CallError{Code: "metrics", Message: err.Error()}), nil
		}
		return reply(m, nil), nil

	case MethodProbe:
		req, err := DecodePayload[ProbeReq](env)
		if err != nil {
			return reply(nil, &CallError{Code: "bad_request", Message: err.Error()}), nil
		}
		return reply(h.runProbe(ctx, req), nil), nil

	case MethodMaterialize:
		req, err := DecodePayload[MaterializeReq](env)
		if err != nil {
			return reply(nil, &CallError{Code: "bad_request", Message: err.Error()}), nil
		}
		fn := h.MaterializeFn
		if fn == nil {
			fn = defaultMaterialize
		}
		return reply(fn(req), nil), nil

	case MethodShutdown:
		req, err := DecodePayload[ShutdownReq](env)
		if err != nil {
			return reply(nil, &CallError{Code: "bad_request", Message: err.Error()}), nil
		}
		// Delete the job keychain before sync and halt. A failure still
		// halts: the pod is already stopping.
		kerr := h.releaseJobKeychain()
		var serr error
		if h.ShutdownFn != nil {
			serr = h.ShutdownFn(ctx, req)
		}
		if err := errors.Join(kerr, serr); err != nil {
			return reply(nil, &CallError{Code: "shutdown", Message: err.Error()}), nil
		}
		return reply(map[string]bool{"ok": true}, nil), nil

	case MethodSelftest:
		req, err := DecodePayload[SelftestReq](env)
		if err != nil {
			return reply(nil, &CallError{Code: "bad_request", Message: err.Error()}), nil
		}
		if h.SelftestFn != nil {
			return reply(h.SelftestFn(req), nil), nil
		}
		return reply(defaultSelftest(req), nil), nil

	case MethodLogs:
		req, _ := DecodePayload[LogsReq](env)
		ch := make(chan Envelope, 16)
		go func() {
			defer close(ch)
			var lines [][]byte
			var notify <-chan logLine
			if req.Follow {
				// Snapshot and subscribe under one lock. A Write between
				// those two steps used to be in neither, so follow dropped it.
				entries, nch, cancel := h.LogBuffer.tailAndSubscribe(req.TailLines)
				notify = nch
				defer cancel()
				lines = make([][]byte, len(entries))
				for i, e := range entries {
					lines[i] = formatLogLine(e.at, e.data, req.Timestamps)
				}
			} else {
				lines = h.LogBuffer.TailFormatted(req.TailLines, req.Timestamps)
			}
			for _, ln := range lines {
				b, _ := json.Marshal(LogsEvent{Line: ln})
				if !sendFrame(ctx, ch, Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindStream, Method: MethodLogs, Payload: b}) {
					return
				}
			}
			if !req.Follow {
				sendFrame(ctx, ch, Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: MethodLogs})
				return
			}
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case rec, ok := <-notify:
					if !ok {
						return
					}
					b, _ := json.Marshal(LogsEvent{Line: formatLogLine(rec.at, rec.data, req.Timestamps)})
					select {
					case ch <- Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindStream, Method: MethodLogs, Payload: b}:
					default:
						// Consumer too slow for follow; drop rather than block.
					}
				case <-ticker.C:
					if ctx.Err() != nil {
						return
					}
				}
			}
		}()
		return Envelope{}, ch

	case MethodExec:
		req, err := DecodePayload[ExecReq](env)
		if err != nil {
			return reply(nil, &CallError{Code: "bad_request", Message: err.Error()}), nil
		}
		release := h.tryExec()
		if release == nil {
			return reply(nil, &CallError{Code: ErrCodeOverloaded, Message: "too many execs"}), nil
		}
		ch := make(chan Envelope, 16)
		interactive := req.Stdin || req.TTY
		var q *stdinQueue
		if interactive && conn != nil {
			q = conn.upstream.get(env.ID)
			if q == nil {
				q = conn.upstream.register(env.ID)
			}
		}
		stdinR := newExecStdinReader(q, interactive)
		if interactive && conn != nil {
			stdinR.onGrant = newWindowSender(conn.fc, env.ID).add
		}
		go func() {
			defer close(ch)
			defer release()
			send := func(ev ExecEvent) {
				b, _ := json.Marshal(ev)
				sendFrame(ctx, ch, Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindStream, Method: MethodExec, Payload: b})
			}
			code, runErr := h.runExec(ctx, req, stdinR, send)
			if runErr != nil && code == 0 {
				code = 1
			}
			b, _ := json.Marshal(ExecEvent{Exited: true, ExitCode: code})
			sendFrame(ctx, ch, Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindStream, Method: MethodExec, Payload: b})
			sendFrame(ctx, ch, Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: MethodExec, Payload: b})
		}()
		return Envelope{}, ch

	case MethodEphemeralKeychain:
		if err := ctx.Err(); err != nil {
			return reply(nil, &CallError{Code: "keychain", Message: err.Error()}), nil
		}
		req, err := DecodePayload[EphemeralKeychainReq](env)
		if err != nil {
			return reply(nil, &CallError{Code: "bad_request", Message: err.Error()}), nil
		}
		if h.OnKeychain != nil {
			h.OnKeychain(ctx, req)
		}
		res, kerr := h.ephemeralKeychain(ctx, req)
		if kerr != nil {
			return reply(nil, &CallError{Code: "keychain", Message: kerr.Error()}), nil
		}
		return reply(res, nil), nil

	case MethodAdopt:
		if err := ctx.Err(); err != nil {
			return reply(nil, &CallError{Code: "adopt", Message: err.Error()}), nil
		}
		req, err := DecodePayload[AdoptReq](env)
		if err != nil {
			return reply(nil, &CallError{Code: "bad_request", Message: err.Error()}), nil
		}
		res, aerr := h.adopt(ctx, req)
		if aerr != nil {
			return reply(nil, &CallError{Code: "adopt", Message: aerr.Error()}), nil
		}
		return reply(res, nil), nil

	case MethodWorkload:
		req, err := DecodePayload[WorkloadReq](env)
		if err != nil {
			return reply(nil, &CallError{Code: "bad_request", Message: err.Error()}), nil
		}
		if h.OnWorkload != nil {
			h.OnWorkload(ctx, req)
		}
		if h.WorkloadFn != nil {
			if err := h.WorkloadFn(ctx, req); err != nil {
				return reply(nil, &CallError{Code: "workload", Message: err.Error()}), nil
			}
			return reply(WorkloadRes{OK: true}, nil), nil
		}
		if err := h.launchWorkload(req); err != nil {
			return reply(nil, &CallError{Code: "workload", Message: err.Error()}), nil
		}
		return reply(WorkloadRes{OK: true}, nil), nil
	}

	return reply(nil, &CallError{Code: "unknown_method", Message: env.Method}), nil
}

func (h *Handler) runExec(ctx context.Context, req ExecReq, stdin io.Reader, emit func(ExecEvent)) (int, error) {
	if len(req.Argv) == 0 {
		return 1, fmt.Errorf("empty argv")
	}
	stdout := &streamWriter{emit: func(b []byte) { emit(ExecEvent{Stdout: append([]byte(nil), b...)}) }}
	stderr := &streamWriter{emit: func(b []byte) { emit(ExecEvent{Stderr: append([]byte(nil), b...)}) }}
	if h.ExecFn != nil {
		return h.ExecFn(ctx, req, stdin, stdout, stderr)
	}
	cmd := exec.CommandContext(ctx, req.Argv[0], req.Argv[1:]...)
	if req.WorkingDir != "" {
		cmd.Dir = req.WorkingDir
	}
	if len(req.Env) > 0 {
		env := os.Environ()
		for k, v := range req.Env {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}

	// TTY: run the process under a real PTY so line discipline, ^C bytes,
	// and isatty-dependent tools behave like an ssh session. stdin frames
	// are written into the PTY master; master output emits as Stdout.
	if req.TTY {
		return h.runExecTTY(ctx, cmd, stdin, stdout)
	}

	cmd.Stdout = stdout
	cmd.Stderr = stderr
	// Do not assign cmd.Stdin to the frame reader. cmd.Run waits for that
	// copy, and the copy stays blocked in Read when the process has already
	// exited and the client has not sent EOF. The exec slot would stay held.
	err := startWithStdin(cmd, stdin)
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return 1, err
}

// startWithStdin runs cmd and copies stdin on the side. When the process
// exits, the stdin reader is aborted so Wait is not stuck behind a client
// that still has the stream open.
func startWithStdin(cmd *exec.Cmd, stdin io.Reader) error {
	if stdin == nil {
		return cmd.Run()
	}
	sr, _ := stdin.(*execStdinReader)
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.Stdin = pr
	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		return err
	}
	// The child has the read end. Closing the parent's copy is what lets
	// the process observe EOF when the copy finishes.
	_ = pr.Close()
	copyDone := make(chan struct{})
	go func() {
		defer close(copyDone)
		_, _ = io.Copy(pw, stdin)
		_ = pw.Close()
	}()
	waitErr := cmd.Wait()
	if sr != nil {
		sr.abortNow()
	}
	<-copyDone
	return waitErr
}

func (h *Handler) runProbe(ctx context.Context, req ProbeReq) ProbeRes {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 1 * time.Second
	}
	if timeout > MaxProbeTimeout {
		timeout = MaxProbeTimeout
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	switch req.Type {
	case ProbeExec:
		release := h.tryExec()
		if release == nil {
			return ProbeRes{OK: false, Message: ErrCodeOverloaded}
		}
		defer release()
		code, err := h.runExec(pctx, ExecReq{Argv: req.Argv}, nil, func(ExecEvent) {})
		if err != nil {
			return ProbeRes{OK: false, Message: err.Error()}
		}
		return ProbeRes{OK: code == 0, Message: fmt.Sprintf("exit %d", code)}
	case ProbeHTTP:
		if h.ProbeHTTPFn != nil {
			return h.ProbeHTTPFn(pctx, req)
		}
		url := req.URL
		if url == "" {
			url = fmt.Sprintf("http://%s:%d/", req.Host, req.Port)
		}
		httpReq, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
		if err != nil {
			return ProbeRes{OK: false, Message: err.Error()}
		}
		for k, v := range req.Headers {
			httpReq.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			return ProbeRes{OK: false, Message: err.Error()}
		}
		defer resp.Body.Close()
		ok := resp.StatusCode >= 200 && resp.StatusCode < 400
		return ProbeRes{OK: ok, StatusCode: resp.StatusCode}
	case ProbeTCP:
		host := req.Host
		if host == "" {
			host = "127.0.0.1"
		}
		var d net.Dialer
		c, err := d.DialContext(pctx, "tcp", fmt.Sprintf("%s:%d", host, req.Port))
		if err != nil {
			return ProbeRes{OK: false, Message: err.Error()}
		}
		_ = c.Close()
		return ProbeRes{OK: true}
	default:
		return ProbeRes{OK: false, Message: "unknown probe type"}
	}
}

func defaultMaterialize(req MaterializeReq) MaterializeRes {
	var placed []string
	root := GuestShareRoot()
	for _, v := range req.Volumes {
		dst, err := placeVolume(root, v)
		if err != nil {
			return MaterializeRes{OK: false, Message: err.Error()}
		}
		placed = append(placed, dst)
	}
	return MaterializeRes{OK: true, Placed: placed}
}

// placeVolume links or copies one share into the guest. It refuses to delete
// a real guest directory: a mountPath of an existing folder must not wipe it
// the way RemoveAll used to. Symlinks we created earlier are replaced.
func placeVolume(root string, v VolumePlace) (string, error) {
	if err := safeShareName(v.Name); err != nil {
		return "", err
	}
	dst, err := cleanGuestTarget(v.GuestPath)
	if err != nil {
		return "", err
	}
	src := filepath.Join(root, v.Name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	switch v.Mode {
	case "copy":
		if err := removeReplacableSymlink(dst); err != nil {
			return "", err
		}
		if err := copyPath(src, dst); err != nil {
			return "", err
		}
	default: // link
		fi, err := os.Lstat(dst)
		if err == nil {
			if fi.Mode()&os.ModeSymlink == 0 {
				return "", fmt.Errorf("refusing to replace existing guest path %s", dst)
			}
			if err := os.Remove(dst); err != nil {
				return "", err
			}
		} else if !os.IsNotExist(err) {
			return "", err
		}
		if err := os.Symlink(src, dst); err != nil {
			// Fallback: copy if symlink is not permitted.
			if cErr := copyPath(src, dst); cErr != nil {
				return "", fmt.Errorf("%w; copy: %v", err, cErr)
			}
		}
	}
	return dst, nil
}

func safeShareName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("invalid volume name %q", name)
	}
	if filepath.Clean(name) != name {
		return fmt.Errorf("invalid volume name %q", name)
	}
	return nil
}

func cleanGuestTarget(p string) (string, error) {
	if p == "" || !strings.HasPrefix(filepath.ToSlash(p), "/") {
		return "", fmt.Errorf("guest path must be absolute")
	}
	for _, seg := range strings.Split(filepath.ToSlash(p), "/") {
		if seg == ".." {
			return "", fmt.Errorf("guest path %q contains ..", p)
		}
	}
	cleaned := filepath.Clean(p)
	switch cleaned {
	case "/", "/Volumes", GuestShareRoot():
		return "", fmt.Errorf("refusing guest path %q", cleaned)
	}
	return cleaned, nil
}

func removeReplacableSymlink(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		return nil
	}
	return os.Remove(path)
}

// GuestShareRoot is the virtio-fs automount.
func GuestShareRoot() string {
	return "/Volumes/My Shared Files"
}

func copyPath(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if info.IsDir() {
		if err := os.MkdirAll(dst, info.Mode()); err != nil {
			return err
		}
		ents, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, e := range ents {
			// Atomic volume publish reserves ".." names (..data, timestamp
			// directories). Copying them would duplicate the payload.
			if strings.HasPrefix(e.Name(), "..") {
				continue
			}
			if err := copyPath(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
				return err
			}
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode())
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// RunSelftest is the CLI entry used by `darwin-guest-agent selftest`.
func RunSelftest(name string) SelftestRes {
	return defaultSelftest(SelftestReq{Name: name})
}

func authorizeHandshake(h Handler, req HandshakeReq) *CallError {
	token := h.Token
	if h.live != nil {
		token = h.live.getToken()
	}
	if h.AllowInsecureNoToken {
		if token != "" && req.Token != token {
			return &CallError{Code: ErrCodeUnauthorized, Message: "bad token"}
		}
		return nil
	}
	if token == "" || req.Token != token {
		return &CallError{Code: ErrCodeUnauthorized, Message: "bad token"}
	}
	return nil
}

func (h *Handler) checkNonce(nonce string) *CallError {
	if h.live == nil {
		h.live = newHandlerLive(h.Token, h.Limiter)
	}
	return h.live.nonces.check(nonce)
}

func (h *Handler) tryExec() func() {
	if h.live == nil || h.live.lim == nil {
		return func() {}
	}
	if !h.live.lim.TryExec() {
		return nil
	}
	return h.live.lim.ReleaseExec
}

func defaultSelftest(req SelftestReq) SelftestRes {
	switch strings.ToLower(req.Name) {
	case "metal":
		if !defaultMetalAvailable() {
			return SelftestRes{OK: false, Message: "no Metal-capable graphics device"}
		}
		return SelftestRes{OK: true, Message: "Metal graphics device present"}
	case "agent":
		return SelftestRes{OK: true, Message: "ok"}
	default:
		return SelftestRes{OK: false, Message: "unknown selftest"}
	}
}

type streamWriter struct {
	emit func([]byte)
}

// maxStreamChunk bounds one exec stdout/stderr frame so a single Write
// cannot be larger than the host flow-control window.
const maxStreamChunk = 32 << 10

func (w *streamWriter) Write(p []byte) (int, error) {
	if w.emit == nil || len(p) == 0 {
		return len(p), nil
	}
	total := len(p)
	for len(p) > 0 {
		n := len(p)
		if n > maxStreamChunk {
			n = maxStreamChunk
		}
		w.emit(p[:n])
		p = p[n:]
	}
	return total, nil
}

// logLine is one ring entry. at is when the bytes were appended, so
// kubectl logs --timestamps can prefix them without storing the prefix.
type logLine struct {
	at   time.Time
	data []byte
}

// LogBuffer is a small in-memory ring of log lines.
type LogBuffer struct {
	mu       sync.Mutex
	n        int
	maxLine  int
	maxBytes int
	nbytes   int
	lines    []logLine
	subs     []chan logLine
}

func NewLogBuffer(n int) *LogBuffer {
	if n <= 0 {
		n = 256
	}
	return &LogBuffer{n: n, maxLine: MaxLogLineBytes, maxBytes: DefaultLogBufferBytes}
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 || b == nil {
		return n, nil
	}
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	maxLine := b.maxLine
	if maxLine <= 0 {
		maxLine = MaxLogLineBytes
	}
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxLine {
			chunk = p[:maxLine]
		}
		p = p[len(chunk):]
		cp := append([]byte(nil), chunk...)
		line := logLine{at: now, data: cp}
		b.lines = append(b.lines, line)
		b.nbytes += len(cp)
		b.dropLocked()
		b.notifyLocked(line)
	}
	return n, nil
}

func (b *LogBuffer) dropLocked() {
	for len(b.lines) > 0 {
		overBytes := b.maxBytes > 0 && b.nbytes > b.maxBytes
		overLines := b.n > 0 && len(b.lines) > b.n
		if !overBytes && !overLines {
			break
		}
		b.nbytes -= len(b.lines[0].data)
		b.lines[0].data = nil
		b.lines = b.lines[1:]
	}
}

func (b *LogBuffer) snapshot(n int) []logLine {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n >= len(b.lines) {
		out := make([]logLine, len(b.lines))
		copy(out, b.lines)
		return out
	}
	out := make([]logLine, n)
	copy(out, b.lines[len(b.lines)-n:])
	return out
}

func (b *LogBuffer) Tail(n int) [][]byte {
	entries := b.snapshot(n)
	out := make([][]byte, len(entries))
	for i, e := range entries {
		out[i] = e.data
	}
	return out
}

// TailFormatted is Tail with an RFC3339Nano prefix on each line when
// timestamps is set. The prefix is the time the line was written.
func (b *LogBuffer) TailFormatted(n int, timestamps bool) [][]byte {
	entries := b.snapshot(n)
	out := make([][]byte, len(entries))
	for i, e := range entries {
		out[i] = formatLogLine(e.at, e.data, timestamps)
	}
	return out
}

func formatLogLine(at time.Time, line []byte, timestamps bool) []byte {
	if !timestamps || at.IsZero() {
		return line
	}
	prefix := at.UTC().Format(time.RFC3339Nano) + " "
	out := make([]byte, 0, len(prefix)+len(line))
	out = append(out, prefix...)
	out = append(out, line...)
	return out
}

// tailAndSubscribe copies the current tail and registers the subscriber
// before releasing the lock. A Write holds the same lock, so it is either
// in the returned tail or delivered on the channel, not lost between them.
// Delivery is best-effort: a subscriber that falls more than its buffer
// behind loses lines rather than blocking writers. cancel releases the slot.
func (b *LogBuffer) tailAndSubscribe(n int) ([]logLine, <-chan logLine, func()) {
	ch := make(chan logLine, 256)
	b.mu.Lock()
	var entries []logLine
	if n <= 0 || n >= len(b.lines) {
		entries = make([]logLine, len(b.lines))
		copy(entries, b.lines)
	} else {
		entries = make([]logLine, n)
		copy(entries, b.lines[len(b.lines)-n:])
	}
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	return entries, ch, b.unsub(ch)
}

func (b *LogBuffer) unsub(ch chan logLine) func() {
	return func() {
		b.mu.Lock()
		for i, c := range b.subs {
			if c == ch {
				b.subs = append(b.subs[:i], b.subs[i+1:]...)
				close(ch)
				break
			}
		}
		b.mu.Unlock()
	}
}

func (b *LogBuffer) notifyLocked(line logLine) {
	for i := 0; i < len(b.subs); i++ {
		select {
		case b.subs[i] <- line:
		default:
			// Subscriber too slow: drop it so follow cannot stall writes.
			close(b.subs[i])
			b.subs = append(b.subs[:i], b.subs[i+1:]...)
			i--
		}
	}
}
