// Package runtime is the VM lifecycle interface. No Kubernetes types.
package runtime

import (
	"context"
	"io"
	"time"

	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/types"
)

// Runtime creates machines. Implementations: vz (darwin/arm64) and fake.
type Runtime interface {
	Name() types.RuntimeName
	Create(ctx context.Context, spec types.VMSpec) (Machine, error)
}

// Machine is one VM instance.
type Machine interface {
	ID() types.MachineID
	Start(ctx context.Context) error
	Stop(ctx context.Context, graceful time.Duration) error
	Status() types.VMStatus
	DialAgent(ctx context.Context) (*guest.Client, error)
	Logs() io.ReadCloser
}

// Consoler is the optional break-glass surface: a raw byte stream to the
// VM's serial console, independent of the in-guest agent and SSH. Machines
// may implement it; callers must type-assert.
type Consoler interface {
	Console() (io.ReadWriteCloser, error)
}

// Options configure a runtime backend.
type Options struct {
	CacheDir     string
	NetworkMode  types.NetworkMode
	BridgeDevice string
	Graphics     types.Graphics
	HostAgentVer string
	// SerialConsole attaches a VM serial port surfaced via Machine's
	// Consoler interface (break-glass access independent of the agent).
	SerialConsole bool
}
