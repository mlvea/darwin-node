package engine

import (
	"sync/atomic"
	"time"
)

// probeObserver records one probe sample. main points it at the
// Prometheus histograms. A nil installation is a no-op.
var probeObserver atomic.Value

func init() {
	probeObserver.Store(func(string, time.Duration, error) {})
}

// SetProbeObserver installs fn. A nil fn clears the hook. kind is
// startup, readiness, liveness, or sidecar.
func SetProbeObserver(fn func(kind string, d time.Duration, err error)) {
	if fn == nil {
		fn = func(string, time.Duration, error) {}
	}
	probeObserver.Store(fn)
}

func observeProbe(kind string, start time.Time, err error) {
	probeObserver.Load().(func(string, time.Duration, error))(kind, time.Since(start), err)
}
