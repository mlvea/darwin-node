package guest

import (
	"context"
	"encoding/json"
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
	// WriteTimeout is applied per frame write (default 30s).
	WriteTimeout time.Duration

	live *handlerLive

	// Hooks allow tests to stub OS work. Nil → real implementation.
	ExecFn           func(ctx context.Context, req ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	ProbeHTTPFn      func(ctx context.Context, req ProbeReq) ProbeRes
	NetInfoFn        func() (NetInfoRes, error)
	MetricsFn        func() (MetricsRes, error)
	MaterializeFn    func(req MaterializeReq) MaterializeRes
	ShutdownFn       func(ctx context.Context, req ShutdownReq) error
	SelftestFn       func(req SelftestReq) SelftestRes
	MetalAvailableFn func() bool
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
	var wg sync.WaitGroup
	defer wg.Wait()

	inflight := newSemaphore(DefaultMaxInflight)
	conn := &connState{upstream: newUpstreamRouter()}

	setConnDeadline(rw, h.handshakeTimeout())

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// An interactive exec may legitimately stay silent longer than the
		// idle timeout (an open shell produces no frames until output).
		// Suppress the deadline while any is running.
		if handshook.Load() {
			if conn.liveInteractive.Load() > 0 {
				setReadDeadline(rw, 0)
			} else {
				setReadDeadline(rw, h.idleTimeout())
			}
		}
		env, err := fc.Read()
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		// Client→agent stream frames (stdin, tty resize) route to the
		// goroutine serving that envelope ID. Unknown IDs are dropped.
		if env.Kind == KindStream {
			conn.upstream.forward(env)
			continue
		}
		if env.Kind != KindRequest {
			continue
		}

		// Handshake stays serial so later methods cannot race the token check.
		if env.Method == MethodHandshake || !handshook.Load() {
			if err := h.writeDispatch(ctx, fc, &handshook, env, conn); err != nil {
				return err
			}
			if handshook.Load() {
				setConnDeadline(rw, 0)
			}
			continue
		}

		if !inflight.TryAcquire() {
			_ = fc.Write(overloadedEnvelope(env, "too many in-flight requests"))
			continue
		}
		wg.Add(1)
		go func(env Envelope) {
			defer wg.Done()
			defer inflight.Release()
			_ = h.writeDispatch(ctx, fc, &handshook, env, conn)
		}(env)
	}
}

// upstreamRouter delivers client→agent stream frames to the exec goroutine
// that owns the envelope ID. Delivery is best-effort with a bounded buffer;
// a slow consumer drops data rather than stalling the read loop.
type upstreamRouter struct {
	mu      sync.Mutex
	streams map[string]chan Envelope
}

// connState carries per-connection state into dispatch goroutines.
type connState struct {
	upstream        *upstreamRouter
	liveInteractive atomic.Int32
}

func newUpstreamRouter() *upstreamRouter {
	return &upstreamRouter{streams: map[string]chan Envelope{}}
}

func (u *upstreamRouter) register(id string) <-chan Envelope {
	ch := make(chan Envelope, 64)
	u.mu.Lock()
	u.streams[id] = ch
	u.mu.Unlock()
	return ch
}

func (u *upstreamRouter) unregister(id string) {
	u.mu.Lock()
	delete(u.streams, id)
	u.mu.Unlock()
}

func (u *upstreamRouter) forward(env Envelope) {
	u.mu.Lock()
	ch, ok := u.streams[env.ID]
	u.mu.Unlock()
	if !ok {
		return
	}
	select {
	case ch <- env:
	default:
		select {
		case ch <- Envelope{ID: env.ID, Kind: KindStream, Method: env.Method, Payload: mustJSON(ExecStdin{EOF: true})}:
		default:
		}
	}
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func (h *Handler) writeDispatch(ctx context.Context, fc *FrameConn, handshook *atomic.Bool, env Envelope, conn *connState) error {
	res, stream := h.dispatch(ctx, handshook, env, conn)
	if stream != nil {
		for ev := range stream {
			if err := fc.Write(ev); err != nil {
				return err
			}
		}
		return nil
	}
	return fc.Write(res)
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
			Protocol:       ProtocolVersion,
			MetalAvailable: metal,
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
		if h.ShutdownFn != nil {
			if err := h.ShutdownFn(ctx, req); err != nil {
				return reply(nil, &CallError{Code: "shutdown", Message: err.Error()}), nil
			}
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
			lines := h.LogBuffer.Tail(req.TailLines)
			for _, ln := range lines {
				b, _ := json.Marshal(LogsEvent{Line: ln})
				ch <- Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindStream, Method: MethodLogs, Payload: b}
			}
			if !req.Follow {
				ch <- Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: MethodLogs}
				return
			}
			// Follow: stream appended lines until the connection ends.
			notify, cancel := h.LogBuffer.Subscribe()
			defer cancel()
			ticker := time.NewTicker(200 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case ln, ok := <-notify:
					if !ok {
						return
					}
					b, _ := json.Marshal(LogsEvent{Line: ln})
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
		var upstreamCh <-chan Envelope
		interactive := req.Stdin || req.TTY
		if interactive && conn != nil {
			upstreamCh = conn.upstream.register(env.ID)
			conn.liveInteractive.Add(1)
			go func() {
				// Keep the router entry alive until the stream channel closes.
				for range ch {
				}
				conn.liveInteractive.Add(-1)
				conn.upstream.unregister(env.ID)
			}()
		}
		stdinR := newExecStdinReader(upstreamCh, interactive)
		go func() {
			defer close(ch)
			defer release()
			code, runErr := h.runExec(ctx, req, stdinR, func(ev ExecEvent) {
				b, _ := json.Marshal(ev)
				ch <- Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindStream, Method: MethodExec, Payload: b}
			})
			if runErr != nil && code == 0 {
				code = 1
			}
			b, _ := json.Marshal(ExecEvent{Exited: true, ExitCode: code})
			ch <- Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindStream, Method: MethodExec, Payload: b}
			ch <- Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: MethodExec, Payload: b}
		}()
		return Envelope{}, ch
	}

	return reply(nil, &CallError{Code: "unknown_method", Message: env.Method}), nil
}

// execStdinReader adapts upstream ExecStdin/TtyResize frames into an
// io.Reader for the exec'd process. Resize frames are surfaced on the
// optional resize channel and never enter the byte stream. Abort wakes any
// blocked Read once the command is done, so nothing lingers per session.
type execStdinReader struct {
	ch          <-chan Envelope
	resize      chan<- TtyResize
	abort       chan struct{}
	abortOnce   sync.Once
	current     []byte
	eof         bool
	sawUpstream bool
}

func newExecStdinReader(ch <-chan Envelope, interactive bool) *execStdinReader {
	return &execStdinReader{ch: ch, eof: !interactive, abort: make(chan struct{})}
}

func (r *execStdinReader) abortNow() { r.abortOnce.Do(func() { close(r.abort) }) }

func (r *execStdinReader) pump() {
	for {
		var env Envelope
		ok := false
		select {
		case env, ok = <-r.ch:
		case <-r.abort:
			r.eof = true
			return
		}
		if !ok {
			// Upstream channel closed without EOF (client vanished): end stdin.
			r.eof = true
			return
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
		r.sawUpstream = true
		if len(ev.Data) > 0 {
			r.current = append(r.current, ev.Data...)
		}
		if ev.EOF {
			r.eof = true
			return
		}
		if len(r.current) > 0 {
			return // yield buffered bytes before blocking again
		}
	}
}

func (r *execStdinReader) Read(p []byte) (int, error) {
	if r.ch == nil {
		return 0, io.EOF
	}
	for len(r.current) == 0 {
		if r.eof {
			return 0, io.EOF
		}
		// pump blocks until data, an EOF marker, upstream close, or abort.
		r.pump()
		if r.eof && len(r.current) == 0 {
			return 0, io.EOF
		}
	}
	n := copy(p, r.current)
	r.current = r.current[n:]
	return n, nil
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
	if stdin != nil {
		cmd.Stdin = stdin
	}
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return 1, err
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
		src := filepath.Join(root, v.Name)
		if err := os.MkdirAll(filepath.Dir(v.GuestPath), 0o755); err != nil {
			return MaterializeRes{OK: false, Message: err.Error()}
		}
		switch v.Mode {
		case "copy":
			if err := copyPath(src, v.GuestPath); err != nil {
				return MaterializeRes{OK: false, Message: err.Error()}
			}
		default: // link
			_ = os.RemoveAll(v.GuestPath)
			if err := os.Symlink(src, v.GuestPath); err != nil {
				// Fallback: copy if symlink is not permitted.
				if cErr := copyPath(src, v.GuestPath); cErr != nil {
					return MaterializeRes{OK: false, Message: err.Error() + "; copy: " + cErr.Error()}
				}
			}
		}
		placed = append(placed, v.GuestPath)
	}
	return MaterializeRes{OK: true, Placed: placed}
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

func (w *streamWriter) Write(p []byte) (int, error) {
	if w.emit != nil && len(p) > 0 {
		w.emit(p)
	}
	return len(p), nil
}

// LogBuffer is a small in-memory ring of log lines.
type LogBuffer struct {
	mu       sync.Mutex
	n        int
	maxLine  int
	maxBytes int
	nbytes   int
	lines    [][]byte
	subs     []chan []byte
}

func NewLogBuffer(n int) *LogBuffer {
	if n <= 0 {
		n = 256
	}
	return &LogBuffer{n: n, maxLine: MaxLogLineBytes, maxBytes: DefaultLogBufferBytes}
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}
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
		b.lines = append(b.lines, cp)
		b.nbytes += len(cp)
		b.dropLocked()
		b.notifyLocked(cp)
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
		b.nbytes -= len(b.lines[0])
		b.lines[0] = nil
		b.lines = b.lines[1:]
	}
}

func (b *LogBuffer) Tail(n int) [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n <= 0 || n >= len(b.lines) {
		out := make([][]byte, len(b.lines))
		copy(out, b.lines)
		return out
	}
	out := make([][]byte, n)
	copy(out, b.lines[len(b.lines)-n:])
	return out
}

// Subscribe returns a channel receiving every line appended from now on.
// Delivery is best-effort: a subscriber that falls more than its buffer
// behind loses lines rather than blocking writers. cancel releases the slot.
func (b *LogBuffer) Subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, 256)
	b.mu.Lock()
	b.subs = append(b.subs, ch)
	b.mu.Unlock()
	cancel := func() {
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
	return ch, cancel
}

func (b *LogBuffer) notifyLocked(line []byte) {
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
