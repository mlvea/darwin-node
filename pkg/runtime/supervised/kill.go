package supervised

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mlvea/darwin-node/pkg/supervisor"
	"golang.org/x/sys/unix"
)

// agentStopBudget is how long the node-agent waits for StopVM. Grace is
// the termination period forwarded to the supervisor. The hang cap after
// that grace is 12s, unless override is positive. The supervisor's own
// cap is grace plus 10s, so a hung stop exits the supervisor before this
// process signals it.
func agentStopBudget(grace, override time.Duration) time.Duration {
	hang := override
	if hang <= 0 {
		hang = 12 * time.Second
	}
	if grace < 0 {
		grace = 0
	}
	return grace + hang
}

// enforceStop runs stop until limit. When that deadline is this call's
// own timeout, it confirms ident, SIGKILLs that pid, and waits until the
// process has exited before returning. A parent cancellation is returned
// as-is. A fence mismatch or a pid that is already gone is not signalled.
// The slot must stay reserved until this returns.
func enforceStop(ctx context.Context, limit time.Duration, ident supervisor.ProcessIdentity, stop func(context.Context) error) error {
	if stop == nil {
		return fmt.Errorf("stop is nil")
	}
	if limit <= 0 {
		limit = 12 * time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	err := stop(sctx)
	if err == nil {
		return nil
	}
	if !errors.Is(sctx.Err(), context.DeadlineExceeded) {
		return err
	}
	if cerr := ident.Confirm(); cerr != nil {
		if errors.Is(cerr, supervisor.ErrProcGone) {
			return err
		}
		return cerr
	}
	if ident.PID <= 0 {
		return err
	}
	if kerr := unix.Kill(ident.PID, unix.SIGKILL); kerr != nil && !errors.Is(kerr, unix.ESRCH) {
		return errors.Join(err, kerr)
	}
	if werr := supervisor.WaitExit(ident.PID); werr != nil {
		return errors.Join(err, werr)
	}
	return err
}
