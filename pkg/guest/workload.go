package guest

import (
	"context"
	"fmt"
	"os/exec"
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
	cmd := exec.Command(argv[0], argv[1:]...)
	err := cmd.Run()
	ReportProcessExit(ctx, h, err, timeout, halt)
	return err
}

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
