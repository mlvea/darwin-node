package simvz

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
)

// defaultIdle is the protocol idle timeout. Compared by value so a scaled
// clock's shorter idle is the one passed to the child.
var defaultIdle = guest.DefaultIdleTimeout

var (
	guestOnce sync.Once
	guestBin  string
	guestErr  error
)

// GuestAgentBinary builds cmd/guest-agent once per process and returns the
// executable path. Tests pass that path as Options.GuestBinary.
func GuestAgentBinary(ctx context.Context) (string, error) {
	guestOnce.Do(func() {
		guestBin, guestErr = buildGuestAgent(ctx)
	})
	return guestBin, guestErr
}

func buildGuestAgent(ctx context.Context) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
	}
	dir, err := os.MkdirTemp("", "simvz-agent-")
	if err != nil {
		return "", err
	}
	bin := filepath.Join(dir, "darwin-guest-agent")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, "github.com/mlvea/darwin-node/cmd/guest-agent")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build guest agent: %w: %s", err, out)
	}
	return bin, nil
}
