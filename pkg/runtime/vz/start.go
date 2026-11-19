package vz

import (
	"io"
	"time"
)

// finishStart closes the vsock listener and the serial console when Start
// fails. Start opens the listener before vm.Start, and Create has already
// opened the console pipe; returning the error without closing them leaks
// both if the caller never reaches Stop. On success both stay open:
// DialAgent accepts the listener and Console reads the pipe. Close errors
// are dropped. A nil closer is fine. started is set only on success.
func finishStart(startErr error, listener, console io.Closer, now time.Time) (*time.Time, error) {
	if startErr != nil {
		if listener != nil {
			_ = listener.Close()
		}
		if console != nil {
			_ = console.Close()
		}
		return nil, startErr
	}
	started := now
	return &started, nil
}
