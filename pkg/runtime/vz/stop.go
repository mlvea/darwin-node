package vz

import (
	"io"
	"time"
)

// finishStop closes the vsock listener and the serial console on every
// stop path. A stop error is returned as-is and leaves finished unset, so
// a failed Stop is not recorded as a clean shutdown. Close errors are
// dropped; the stop error is the one the caller can act on. Nil closers
// are fine.
func finishStop(stopErr error, listener, console io.Closer, now time.Time) (*time.Time, error) {
	if listener != nil {
		_ = listener.Close()
	}
	if console != nil {
		_ = console.Close()
	}
	if stopErr != nil {
		return nil, stopErr
	}
	finished := now
	return &finished, nil
}
