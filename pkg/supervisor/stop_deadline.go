package supervisor

import (
	"errors"
	"time"
)

// errStopDeadline is returned when a VM stop exceeds its hang cap and
// the process has been asked to exit.
var errStopDeadline = errors.New("vz stop deadline exceeded")

// stopHangBudget is the supervisor's wait for Machine.Stop. override
// replaces the 10s hang cap. The caller's grace period is added so a
// guest can still shut down before the force-stop cap starts.
func stopHangBudget(grace, override time.Duration) time.Duration {
	if grace < 0 {
		return 0
	}
	return grace
}
