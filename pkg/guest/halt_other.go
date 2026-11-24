//go:build !darwin

package guest

import "context"

// HaltGuest is a no-op off Darwin. Linux tests pass their own halt func
// and must not shut the machine down.
func HaltGuest() {}

// OSStopper is a no-op off Darwin. The Shutdown RPC still runs the hook;
// it does not power off the machine the tests are running on.
type OSStopper struct{}

func (OSStopper) Sync() error { return nil }

func (OSStopper) Halt(context.Context) error { return nil }
