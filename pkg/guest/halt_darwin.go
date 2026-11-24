//go:build darwin

package guest

import (
	"context"
	"os/exec"
	"time"
)

// HaltGuest stops the VM. launchd starts the agent as root, so shutdown
// does not go through sudo. The call is bounded so a stuck shutdown cannot
// keep the process after the Exit notify.
func HaltGuest() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(ctx, "/sbin/shutdown", "-h", "now").Run()
}
