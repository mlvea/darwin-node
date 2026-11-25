package simvz

import "errors"

// ErrInvalidState is returned when Start, Stop, Pause, Resume, Save, or
// DialAgent is used in a state that does not allow it.
var ErrInvalidState = errors.New("invalid virtual machine state")

// ErrVirtualMachineLimitExceeded is the simulator's VZErrorVirtualMachineLimitExceeded.
// Apple's framework returns VZErrorDomain code 6 once two macOS VMs are
// already running (Code-Hex/vz ErrorVirtualMachineLimitExceeded, and
// NSError Code=6 in field reports). Code 9 is VZErrorOperationCancelled,
// not the limit. A third Start or Restore fails with this error; Create
// does not.
var ErrVirtualMachineLimitExceeded = errors.New("virtual machine limit exceeded")

// ErrRestore is an incompatible save file or a restore the framework
// rejected. VZErrorDomain code 12 is VZErrorRestore.
var ErrRestore = errors.New("invalid virtual machine restore")

// vzCoded is the NSError shape the contract suite reads back.
type vzCoded interface {
	Domain() string
	Code() int
	FrameworkName() string
}

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

func (e *StateError) Domain() string {
	return "VZErrorDomain"
}

func (e *StateError) Code() int { return 3 }

func (e *StateError) FrameworkName() string {
	return "VZErrorInvalidVirtualMachineState"
}

// limitError carries the framework's domain and code in the text so a log
// line can be matched, and unwraps to ErrVirtualMachineLimitExceeded.
type limitError struct{}

func (limitError) Error() string {
	return "vz virtual machine limit exceeded: VZErrorDomain code 6 VZErrorVirtualMachineLimitExceeded"
}

func (limitError) Unwrap() error { return ErrVirtualMachineLimitExceeded }

func (limitError) Domain() string { return "VZErrorDomain" }

func (limitError) Code() int { return 6 }

func (limitError) FrameworkName() string { return "VZErrorVirtualMachineLimitExceeded" }

// FrameworkError is a VZErrorDomain NSError other than the two sentinels
// above (restore mismatch uses code 12).
type FrameworkError struct {
	DomainName string
	ErrCode    int
	VZName     string
	err        error
}

func (e *FrameworkError) Error() string {
	if e == nil {
		return ErrRestore.Error()
	}
	return "Error Domain=" + e.DomainName + " Code=" + itoa(e.ErrCode) + " " + e.VZName
}

func (e *FrameworkError) Unwrap() error {
	if e == nil || e.err == nil {
		return ErrRestore
	}
	return e.err
}

func (e *FrameworkError) Domain() string {
	if e == nil {
		return "VZErrorDomain"
	}
	return e.DomainName
}

func (e *FrameworkError) Code() int {
	if e == nil {
		return 12
	}
	return e.ErrCode
}

func (e *FrameworkError) FrameworkName() string {
	if e == nil {
		return "VZErrorRestore"
	}
	return e.VZName
}

func restoreFailed(reason error) error {
	return &FrameworkError{
		DomainName: "VZErrorDomain",
		ErrCode:    12,
		VZName:     "VZErrorRestore",
		err:        errors.Join(ErrRestore, reason),
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
