package guest

import (
	"context"
	"errors"
)

// Stopper syncs dirty buffers and then halts the guest. Tests supply a
// fake. The process uses OSStopper.
type Stopper interface {
	Sync() error
	Halt(ctx context.Context) error
}

// ShutdownFunc returns the handler hook for MethodShutdown. Sync runs
// first. Halt runs even when sync fails. The returned error is the first
// of the two, and both when both fail.
func ShutdownFunc(s Stopper) func(ctx context.Context, req ShutdownReq) error {
	return func(ctx context.Context, req ShutdownReq) error {
		if s == nil {
			s = OSStopper{}
		}
		if ctx == nil {
			ctx = context.Background()
		}
		syncErr := s.Sync()
		haltErr := s.Halt(ctx)
		return errors.Join(syncErr, haltErr)
	}
}
