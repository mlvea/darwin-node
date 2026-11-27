package supervised

import (
	"context"
	"time"

	"github.com/mlvea/darwin-node/pkg/supervisor"
)

// agentStopBudget is the node-agent wait for StopVM. override replaces
// the 12s hang cap. Grace is the termination period forwarded to the
// supervisor and is not part of the hang cap.
func agentStopBudget(grace, override time.Duration) time.Duration {
	if grace < 0 {
		return 0
	}
	return grace
}

// enforceStop waits for stop, then SIGKILLs the supervisor and waits
// until the process is gone before returning. The red stub waits on the
// caller's context and does not kill.
func enforceStop(ctx context.Context, limit time.Duration, ident supervisor.ProcessIdentity, stop func(context.Context) error) error {
	_, _ = limit, ident
	if ctx == nil {
		ctx = context.Background()
	}
	return stop(ctx)
}
