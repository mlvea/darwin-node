package image

import (
	"sync/atomic"
	"time"
)

// pullObserver receives one sample per singleflight pull, including a
// pull that fails. Cache hits do not call it. main installs the
// Prometheus observer; tests replace it.
var pullObserver atomic.Value

func init() {
	pullObserver.Store(func(time.Duration, error) {})
}

// SetPullObserver installs fn. A nil fn clears the hook. The hook must
// not block: it runs on the pull goroutine before waiters are released.
func SetPullObserver(fn func(time.Duration, error)) {
	if fn == nil {
		fn = func(time.Duration, error) {}
	}
	pullObserver.Store(fn)
}

func notePull(d time.Duration, err error) {
	pullObserver.Load().(func(time.Duration, error))(d, err)
}
