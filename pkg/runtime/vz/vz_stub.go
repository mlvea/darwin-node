//go:build !darwin || !arm64

package vz

import (
	"context"
	"fmt"

	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/types"
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
