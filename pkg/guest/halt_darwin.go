//go:build darwin

package guest

import (
	"context"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

// OSStopper syncs the guest filesystem and then halts. launchd starts the
// agent as root, so shutdown does not go through sudo.
type OSStopper struct{}

func (OSStopper) Sync() error {
	unix.Sync()
	return nil
}

func (OSStopper) Halt(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return exec.CommandContext(cctx, "/sbin/shutdown", "-h", "now").Run()
}

// HaltGuest stops the VM. launchd starts the agent as root, so shutdown
// does not go through sudo. The call is bounded so a stuck shutdown cannot
// keep the process after the Exit notify.
func HaltGuest() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "/sbin/shutdown", "-h", "now").Run()
}
