package guest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"time"
)

// Client is the host-side agent RPC client.
type Client struct {
	sess  *Session
	info  HandshakeRes
	close io.Closer
}

func newHandshakeNonce() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Dial wraps an established stream (vsock, TCP, or net.Pipe).
func Dial(ctx context.Context, rw io.ReadWriteCloser, token, hostVer string) (*Client, error) {
	nonce, err := newHandshakeNonce()
	if err != nil {
		return nil, err
	}
	s := NewSession(rw)
	c := &Client{sess: s, close: rw}
	env, err := s.Call(ctx, MethodHandshake, HandshakeReq{Token: token, HostAgentVer: hostVer, Nonce: nonce})
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	hs, err := Decode[HandshakeRes](env)
	if err != nil {
		_ = s.Close()
		return nil, err
	}
	if !hs.OK {
		_ = s.Close()
		return nil, fmt.Errorf("handshake rejected")
	}
	c.info = hs
	return c, nil
}

func (c *Client) Info() HandshakeRes { return c.info }

func (c *Client) Health(ctx context.Context) (HealthRes, error) {
	env, err := c.sess.Call(ctx, MethodHealth, nil)
	if err != nil {
		return HealthRes{}, err
	}
	return Decode[HealthRes](env)
}

func (c *Client) Ready(ctx context.Context) (ReadyRes, error) {
	env, err := c.sess.Call(ctx, MethodReady, nil)
	if err != nil {
		return ReadyRes{}, err
	}
	return Decode[ReadyRes](env)
}

func (c *Client) NetInfo(ctx context.Context) (NetInfoRes, error) {
	env, err := c.sess.Call(ctx, MethodNetInfo, nil)
	if err != nil {
		return NetInfoRes{}, err
	}
	return Decode[NetInfoRes](env)
}

func (c *Client) Metrics(ctx context.Context) (MetricsRes, error) {
	env, err := c.sess.Call(ctx, MethodMetrics, nil)
	if err != nil {
		return MetricsRes{}, err
	}
	return Decode[MetricsRes](env)
}

func (c *Client) Probe(ctx context.Context, req ProbeReq) (ProbeRes, error) {
	if req.Timeout == 0 {
		req.Timeout = 1 * time.Second
	}
	env, err := c.sess.Call(ctx, MethodProbe, req)
	if err != nil {
		return ProbeRes{}, err
	}
	return Decode[ProbeRes](env)
}

func (c *Client) Materialize(ctx context.Context, req MaterializeReq) (MaterializeRes, error) {
	env, err := c.sess.Call(ctx, MethodMaterialize, req)
	if err != nil {
		return MaterializeRes{}, err
	}
	return Decode[MaterializeRes](env)
}

func (c *Client) Shutdown(ctx context.Context, req ShutdownReq) error {
	_, err := c.sess.Call(ctx, MethodShutdown, req)
	return err
}

func (c *Client) Selftest(ctx context.Context, name string) (SelftestRes, error) {
	env, err := c.sess.Call(ctx, MethodSelftest, SelftestReq{Name: name})
	if err != nil {
		return SelftestRes{}, err
	}
	return Decode[SelftestRes](env)
}

// Exec runs argv and concatenates stdout/stderr up to MaxExecCapture bytes
// per stream (then truncates). For kubectl exec streaming, use ExecStream.
func (c *Client) Exec(ctx context.Context, req ExecReq) (stdout, stderr []byte, exit int, err error) {
	out := &cappedWriter{limit: MaxExecCapture}
	errOut := &cappedWriter{limit: MaxExecCapture}
	exit, err = c.ExecStream(ctx, req, out, errOut)
	return out.buf, errOut.buf, exit, err
}

// ExecStream writes exec stdout/stderr through without buffering the whole
// payload on the host.
func (c *Client) ExecStream(ctx context.Context, req ExecReq, stdout, stderr io.Writer) (exit int, err error) {
	return c.ExecInteractive(ctx, req, nil, nil, stdout, stderr)
}

// ExecInteractive runs argv with full duplex streaming: stdin bytes are
// forwarded to the guest as they arrive (enabling kubectl exec -it when
// req.TTY is set), resize events reflow the guest PTY, and stdout/stderr
// stream back until exit.
func (c *Client) ExecInteractive(ctx context.Context, req ExecReq, stdin io.Reader, resize <-chan TtyResize, stdout, stderr io.Writer) (exit int, err error) {
	if stdin != nil {
		req.Stdin = true
	}
	id, ch, err := c.sess.StreamWithID(ctx, MethodExec, req)
	if err != nil {
		return -1, err
	}
	if stdin != nil {
		go c.pumpStdin(ctx, id, stdin)
	}
	if resize != nil {
		go c.pumpResize(ctx, id, resize)
	}
	for env := range ch {
		if env.Kind == KindError && env.Error != nil {
			return exit, env.Error
		}
		ev, decErr := Decode[ExecEvent](env)
		if decErr != nil {
			continue
		}
		if len(ev.Stdout) > 0 && stdout != nil {
			if _, err := stdout.Write(ev.Stdout); err != nil {
				return exit, err
			}
		}
		if len(ev.Stderr) > 0 && stderr != nil {
			if _, err := stderr.Write(ev.Stderr); err != nil {
				return exit, err
			}
		}
		if ev.Exited {
			exit = ev.ExitCode
		}
	}
	return exit, nil
}

// pumpStdin forwards a host reader into the running exec as upstream
// stream frames, terminating with an EOF marker.
func (c *Client) pumpStdin(ctx context.Context, id string, stdin io.Reader) {
	defer func() {
		if ctx.Err() == nil {
			_ = c.sess.WriteStream(id, ExecStdin{EOF: true})
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, rerr := stdin.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			if err := c.sess.WriteStream(id, ExecStdin{Data: data}); err != nil {
				return
			}
		}
		if rerr != nil {
			return
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// pumpResize forwards terminal size changes as upstream stream frames.
func (c *Client) pumpResize(ctx context.Context, id string, resize <-chan TtyResize) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-resize:
			if !ok {
				return
			}
			if err := c.sess.WriteStream(id, ev); err != nil {
				return
			}
		}
	}
}

type cappedWriter struct {
	buf   []byte
	limit int
}

func (w *cappedWriter) Write(p []byte) (int, error) {
	if w.limit <= 0 {
		w.buf = append(w.buf, p...)
		return len(p), nil
	}
	remain := w.limit - len(w.buf)
	if remain > 0 {
		if len(p) < remain {
			remain = len(p)
		}
		w.buf = append(w.buf, p[:remain]...)
	}
	return len(p), nil
}

// Logs returns buffered lines (follow is best-effort in this MVP: one snapshot
// plus response). The engine can poll. For true follow, use LogsFollow.
func (c *Client) Logs(ctx context.Context, req LogsReq) ([][]byte, error) {
	ch, err := c.sess.Stream(ctx, MethodLogs, req)
	if err != nil {
		return nil, err
	}
	var lines [][]byte
	for env := range ch {
		if env.Kind == KindResponse || env.Kind == KindError {
			if env.Error != nil {
				return lines, env.Error
			}
			break
		}
		ev, err := Decode[LogsEvent](env)
		if err != nil {
			continue
		}
		if len(ev.Line) > 0 {
			lines = append(lines, ev.Line)
		}
	}
	return lines, nil
}

// LogsFollow streams log lines into w: tail snapshot first, then every
// appended line until ctx is done or the agent closes the stream.
func (c *Client) LogsFollow(ctx context.Context, req LogsReq, w io.Writer) error {
	if !req.Follow {
		req.Follow = true
	}
	ch, err := c.sess.Stream(ctx, MethodLogs, req)
	if err != nil {
		return err
	}
	for env := range ch {
		if env.Kind == KindError && env.Error != nil {
			return env.Error
		}
		if env.Kind == KindResponse {
			return nil
		}
		ev, err := Decode[LogsEvent](env)
		if err != nil || len(ev.Line) == 0 {
			continue
		}
		if _, err := w.Write(ev.Line); err != nil {
			return err
		}
		if ev.Line[len(ev.Line)-1] != '\n' {
			if _, err := w.Write([]byte("\n")); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Client) Close() error {
	return c.sess.Close()
}
