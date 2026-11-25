//go:build !darwin || !arm64

package vz

import (
	"context"
	"fmt"

	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/types"
)

// Runtime is a stub on non-Apple-Silicon platforms.
type Runtime struct {
	Opts runtime.Options
}

func New(opts runtime.Options) *Runtime { return &Runtime{Opts: opts} }

func (r *Runtime) Name() types.RuntimeName { return types.RuntimeVZ }

func (r *Runtime) Create(context.Context, types.VMSpec) (runtime.Machine, error) {
	return nil, fmt.Errorf("vz runtime requires darwin/arm64")
}

// Available reports that this process cannot boot a Virtualization.framework VM.
func Available() error {
	return fmt.Errorf("vz runtime requires darwin/arm64")
}
