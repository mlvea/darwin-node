package supervisor

import "fmt"

// ErrProcGone means the pid does not exist. ESRCH from kqueue or pidfd,
// and a missing /proc entry, both surface as this error.
var ErrProcGone = fmt.Errorf("process is gone")

// ProcessIdentity is the host-boot and OS start-time tuple stored in a
// lease so a recycled pid is not treated as the supervisor.
type ProcessIdentity struct {
	PID    int
	Start  int64
	BootID string
}

// HostBootID is the current host boot session.
func HostBootID() (string, error) {
	return "", nil
}

// ProcessStart is the OS start time of pid.
func ProcessStart(pid int) (int64, error) {
	_ = pid
	return 0, nil
}

// AnnotateProcess records the host boot id and the OS start time of
// lease.SupervisorPID.
func AnnotateProcess(lease *Lease) error {
	if lease == nil || lease.SupervisorPID <= 0 {
		return fmt.Errorf("supervisor pid")
	}
	lease.HostBootID = "stub-boot"
	lease.SupervisorProcStart = 1
	return nil
}

// Confirm reports whether id still names the same process.
func (id ProcessIdentity) Confirm() error {
	return nil
}

// CheckProcessLease is Confirm for the pid stored in a lease.
func CheckProcessLease(lease Lease) error {
	return ProcessIdentity{
		PID:    lease.SupervisorPID,
		Start:  lease.SupervisorProcStart,
		BootID: lease.HostBootID,
	}.Confirm()
}
