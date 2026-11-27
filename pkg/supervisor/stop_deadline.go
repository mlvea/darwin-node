package supervisor

import (
	"errors"
	"time"
)

// errStopDeadline is returned when a VM stop exceeds its hang cap and
// the process has been asked to exit.
var errStopDeadline = errors.New("vz stop deadline exceeded")

// stopHangBudget is how long the supervisor waits for Machine.Stop.
// The caller's grace period is included so a guest can finish its
// termination grace. The hang cap after that grace is 10s, unless
// override is positive, in which case override replaces only the hang
// cap. A hung Virtualization.framework stop is what the cap bounds.
func stopHangBudget(grace, override time.Duration) time.Duration {
	hang := override
	if hang <= 0 {
		hang = 10 * time.Second
	}
	if grace < 0 {
		grace = 0
	}
	return grace + hang
}
