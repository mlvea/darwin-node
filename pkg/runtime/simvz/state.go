package simvz

import "github.com/mlvea/darwin-node/pkg/types"

// vzState is the Virtualization.framework machine state. Names match the
// suffix of vz.VirtualMachineState (Stopped, Starting, Running, Pausing,
// Paused, Saving, Stopping, Resuming, Restoring, Error).
//
// Quota is held in every state except Stopped and Error. Save stays
// Paused; it does not release the slot. Restore ends Paused.
type vzState int

const (
	vzStopped vzState = iota
	vzStarting
	vzRunning
	vzPausing
	vzPaused
	vzSaving
	vzStopping
	vzResuming
	vzRestoring
	vzError
)

func (s vzState) String() string {
	switch s {
	case vzStopped:
		return "Stopped"
	case vzStarting:
		return "Starting"
	case vzRunning:
		return "Running"
	case vzPausing:
		return "Pausing"
	case vzPaused:
		return "Paused"
	case vzSaving:
		return "Saving"
	case vzStopping:
		return "Stopping"
	case vzResuming:
		return "Resuming"
	case vzRestoring:
		return "Restoring"
	case vzError:
		return "Error"
	default:
		return "Error"
	}
}

// holdsQuota reports whether a VM in this state counts against the
// two-VM cap. Only Stopped and Error release it.
func (s vzState) holdsQuota() bool {
	switch s {
	case vzStopped, vzError:
		return false
	default:
		return true
	}
}

// exposed maps a framework state onto the smaller set types.VMState knows.
// Pausing, Paused, Saving, and Resuming are still a live VM. Stopping is
// on the way down, which noteVMDeath does not treat as an unexpected stop.
// Restoring is a stopped VM coming back. Error is a failed VM with no slot.
func (s vzState) exposed() types.VMState {
	switch s {
	case vzStarting, vzRestoring:
		return types.VMStarting
	case vzRunning, vzPausing, vzPaused, vzSaving, vzResuming:
		return types.VMRunning
	case vzStopping:
		return types.VMStopping
	case vzStopped:
		return types.VMStopped
	case vzError:
		return types.VMFailed
	default:
		return types.VMFailed
	}
}
