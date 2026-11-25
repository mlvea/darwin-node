package simvz

import "errors"

// ErrInvalidState is returned when Start, Stop, Pause, Resume, Save, or
// DialAgent is used in a state that does not allow it.
var ErrInvalidState = errors.New("invalid virtual machine state")

// ErrVirtualMachineLimitExceeded is the simulator's VZErrorVirtualMachineLimitExceeded.
// Apple's framework returns VZErrorDomain code 6 once two macOS VMs are
// already running. A third Start fails with this error; Create does not.
var ErrVirtualMachineLimitExceeded = errors.New("virtual machine limit exceeded")

// StateError is a refused transition. Op is the call (start, stop, pause,
// resume, save, dial). State is the VZ state the machine was in.
type StateError struct {
	Op    string
	State string
}

func (e *StateError) Error() string {
	if e == nil {
		return ErrInvalidState.Error()
	}
	return "invalid virtual machine state: cannot " + e.Op + " while " + e.State
}

func (e *StateError) Unwrap() error { return ErrInvalidState }

// limitError carries the framework's domain and code in the text so a log
// line can be matched, and unwraps to ErrVirtualMachineLimitExceeded.
type limitError struct{}

func (limitError) Error() string {
	return "vz virtual machine limit exceeded: VZErrorDomain code 6 VZErrorVirtualMachineLimitExceeded"
}

func (limitError) Unwrap() error { return ErrVirtualMachineLimitExceeded }
