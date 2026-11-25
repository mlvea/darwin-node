package simvz

import "github.com/mlvea/darwin-node/pkg/types"

// vzState is the slice of the Virtualization.framework machine states the
// runtime interface can reach: Stopped, Starting, Running, Pausing, Paused,
// Saving, Stopping. Resuming is not a separate state here; Resume moves
// Paused through Starting back to Running.
type vzState int

const (
	vzStopped vzState = iota
	vzStarting
	vzRunning
	vzPausing
	vzPaused
	vzSaving
	vzStopping
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
	default:
		return "Stopped"
	}
}

// exposed maps a framework state onto the smaller set types.VMState knows.
// Pausing and Paused are still a live VM. Saving and Stopping are on the
// way down, which noteVMDeath does not treat as an unexpected stop.
func (s vzState) exposed() types.VMState {
	switch s {
	case vzStarting:
		return types.VMStarting
	case vzRunning, vzPausing, vzPaused:
		return types.VMRunning
	case vzSaving, vzStopping:
		return types.VMStopping
	case vzStopped:
		return types.VMStopped
	default:
		return types.VMFailed
	}
}
