package supervisor

import (
	"errors"
	"fmt"
)

// ErrProcGone means the pid does not exist. ESRCH from kqueue or pidfd,
// and a missing /proc entry, both surface as this error.
var ErrProcGone = errors.New("process is gone")

// ProcessIdentity is the host-boot and OS start-time tuple stored in a
// lease so a recycled pid is not treated as the supervisor.
type ProcessIdentity struct {
	PID    int
	Start  int64
	BootID string
}

// AnnotateProcess records the host boot id and the OS start time of
// lease.SupervisorPID. An empty boot id or a missing process is an error.
func AnnotateProcess(lease *Lease) error {
	if lease == nil || lease.SupervisorPID <= 0 {
		return fmt.Errorf("supervisor pid")
	}
	boot, err := HostBootID()
	if err != nil {
		return err
	}
	if boot == "" {
		return fmt.Errorf("host boot id is empty")
	}
	start, err := ProcessStart(lease.SupervisorPID)
	if err != nil {
		return err
	}
	if start == 0 {
		return fmt.Errorf("supervisor process start time is empty")
	}
	lease.HostBootID = boot
	lease.SupervisorProcStart = start
	return nil
}

// Confirm reports whether id still names the same process. A different
// host boot, a different OS start time, or a missing process is a fence
// rejection. The caller must not signal the pid after a rejection.
func (id ProcessIdentity) Confirm() error {
	if id.PID <= 0 || id.BootID == "" || id.Start == 0 {
		return fmt.Errorf("%w: supervisor identity", ErrFence)
	}
	boot, err := HostBootID()
	if err != nil {
		return err
	}
	if boot == "" || boot != id.BootID {
		return fmt.Errorf("%w: host boot id", ErrFence)
	}
	start, err := ProcessStart(id.PID)
	if err != nil {
		if errors.Is(err, ErrProcGone) {
			return fmt.Errorf("%w: supervisor process: %w", ErrFence, err)
		}
		return err
	}
	if start != id.Start {
		return fmt.Errorf("%w: supervisor pid reused", ErrFence)
	}
	return nil
}

// CheckProcessLease is Confirm for the pid stored in a lease. An empty
// host boot id fails closed, before the pid is inspected.
func CheckProcessLease(lease Lease) error {
	return ProcessIdentity{
		PID:    lease.SupervisorPID,
		Start:  lease.SupervisorProcStart,
		BootID: lease.HostBootID,
	}.Confirm()
}
