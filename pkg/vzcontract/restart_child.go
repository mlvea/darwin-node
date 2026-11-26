package vzcontract

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/supervisor"
)

const (
	envSupervisorChild  = "DARWIN_NODE_SUPERVISOR_CHILD"
	envSupervisorRunDir = "DARWIN_NODE_SUPERVISOR_RUN_DIR"
	envSupervisorSlot   = "DARWIN_NODE_SUPERVISOR_SLOT"
)

// ChildEnabled reports whether this process was re-exec'd as the slot
// supervisor. TestMain checks it before the suite runs.
func ChildEnabled() bool {
	return os.Getenv(envSupervisorChild) == "1"
}

// ServeChild runs one supervisor until SIGTERM, SIGINT, or the VM stops.
// SIGKILL is the test's stand-in for a crashed supervisor: the kernel
// closes the control socket and the guest fd with no user-space cleanup.
func ServeChild(rt runtime.Runtime) error {
	if rt == nil {
		return fmt.Errorf("supervisor child runtime is nil")
	}
	dir := os.Getenv(envSupervisorRunDir)
	if dir == "" {
		return fmt.Errorf("supervisor run dir is empty")
	}
	slot := 0
	if v := os.Getenv(envSupervisorSlot); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("supervisor slot: %w", err)
		}
		slot = n
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return (&supervisor.Server{
		Slot:          slot,
		RunDir:        dir,
		RT:            rt,
		BinaryVersion: "sv-child",
		Deadman:       time.Hour,
	}).Serve(ctx)
}
