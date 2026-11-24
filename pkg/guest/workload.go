package guest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultExitSendTimeout is how long the agent waits for the host to take
// an Exit notify before it halts anyway. A dead connection cannot hold the
// halt longer than this, plus one more wait of the same length while the
// blocked write observes the close.
const DefaultExitSendTimeout = 2 * time.Second

// Supervise runs argv as the main workload. When the process exits, the
// agent writes an Exit notify with the real exit code and then calls halt.
// A signal death is 128+N. The send is bounded by timeout.
func Supervise(ctx context.Context, h *Handler, argv []string, timeout time.Duration, halt func()) error {
	if len(argv) == 0 || argv[0] == "" {
		return fmt.Errorf("workload command is empty")
	}
	if h != nil {
		h.Init()
		if !h.live.claimWorkload() {
			return fmt.Errorf("workload already started")
		}
	}
	err := runLogged(h, argv, nil, "")
	ReportProcessExit(ctx, h, err, timeout, halt)
	return err
}

// launchWorkload starts the VM container command from the Workload RPC.
// The RPC returns once the process is running. Exit is reported later,
// then the guest halts, the same as Supervise.
func (h *Handler) launchWorkload(req WorkloadReq) error {
	if len(req.Argv) == 0 || req.Argv[0] == "" {
		return fmt.Errorf("workload command is empty")
	}
	if err := validateWorkloadEnv(req.Env); err != nil {
		return err
	}
	if h != nil {
		h.Init()
		if !h.live.claimWorkload() {
			return fmt.Errorf("workload already started")
		}
	}
	go func() {
		err := runLogged(h, req.Argv, req.Env, req.Dir)
		ReportProcessExit(context.Background(), h, err, DefaultExitSendTimeout, HaltGuest)
	}()
	return nil
}

func validateWorkloadEnv(env map[string]string) error {
	for k, v := range env {
		if k == "" || strings.ContainsAny(k, "=\x00") {
			return fmt.Errorf("env %q: invalid name", k)
		}
		if strings.ContainsRune(v, 0) {
			return fmt.Errorf("env %s: value contains NUL", k)
		}
	}
	return nil
}

func runLogged(h *Handler, argv []string, extra map[string]string, dir string) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(extra) > 0 {
		cmd.Env = mergeEnv(os.Environ(), extra)
	}
	var buf *LogBuffer
	if h != nil {
		buf = h.LogBuffer
	}
	stdout := newLogLineWriter(buf)
	stderr := newLogLineWriter(buf)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	stdout.Flush()
	stderr.Flush()
	return err
}

func mergeEnv(base []string, extra map[string]string) []string {
	out := make([]string, 0, len(base)+len(extra))
	for _, kv := range base {
		k, _, ok := strings.Cut(kv, "=")
		if ok {
			if _, drop := extra[k]; drop {
				continue
			}
		}
		out = append(out, kv)
	}
	for k, v := range extra {
		out = append(out, k+"="+v)
	}
	return out
}

// logLineWriter splits a process stream into lines for the agent log ring.
// stdout and stderr each have their own writer so a partial line from one
// does not glue onto the other. A trailing partial is flushed when the
// process exits.
type logLineWriter struct {
	buf  *LogBuffer
	rest []byte
}

func newLogLineWriter(buf *LogBuffer) *logLineWriter {
	return &logLineWriter{buf: buf}
}

func (w *logLineWriter) Write(p []byte) (int, error) {
	if w == nil || w.buf == nil || len(p) == 0 {
		return len(p), nil
	}
	w.rest = append(w.rest, p...)
	for {
		i := bytes.IndexByte(w.rest, '\n')
		if i < 0 {
			break
		}
		line := append([]byte(nil), w.rest[:i+1]...)
		_, _ = w.buf.Write(line)
		w.rest = append([]byte(nil), w.rest[i+1:]...)
	}
	max := MaxLogLineBytes
	if max <= 0 {
		max = 256 << 10
	}
	for len(w.rest) > max {
		_, _ = w.buf.Write(w.rest[:max])
		w.rest = append([]byte(nil), w.rest[max:]...)
	}
	return len(p), nil
}

func (w *logLineWriter) Flush() {
	if w == nil || w.buf == nil || len(w.rest) == 0 {
		return
	}
	_, _ = w.buf.Write(w.rest)
	w.rest = nil
}

// Compile-time check that the writer is an io.Writer.
var _ io.Writer = (*logLineWriter)(nil)

// ReportProcessExit sends the workload's exit code and then halts.
// timeout bounds the send. Halt runs after the frame is written, or after
// the wait expires, including when the host never accepts the frame.
func ReportProcessExit(ctx context.Context, h *Handler, waitErr error, timeout time.Duration, halt func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		timeout = DefaultExitSendTimeout
	}
	code, reason := ExitFromProcess(waitErr)
	reason = NormalizeExitReason(reason)
	if h != nil {
		h.Init()
		done := h.live.publishExitWait(ExitReport{ExitCode: &code, Reason: reason})
		waitExitDelivered(ctx, done, timeout, h.live.closeExitConns)
	}
	if halt != nil {
		halt()
	}
}

// waitExitDelivered returns when done is closed, ctx ends, or timeout
// elapses. After a timeout or cancellation it closes live connections so a
// write blocked on a dead host returns, then waits at most timeout more.
func waitExitDelivered(ctx context.Context, done <-chan struct{}, timeout time.Duration, unblock func() int) {
	if done == nil {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-timer.C:
	case <-ctx.Done():
	}
	if unblock != nil && unblock() > 0 {
		grace := time.NewTimer(timeout)
		defer grace.Stop()
		select {
		case <-done:
		case <-grace.C:
		}
	}
}
