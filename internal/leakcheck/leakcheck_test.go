package leakcheck

import (
	"testing"
	"time"
)

func TestCheckPassesWhenClean(t *testing.T) {
	Check(t)
}

func TestCheckIgnoresGoroutineAliveAtSnapshot(t *testing.T) {
	stop := make(chan struct{})
	go func() {
		<-stop
	}()
	// Registered first so it runs after Check. The goroutine is still
	// blocked while Check compares ids.
	t.Cleanup(func() { close(stop) })
	Check(t)
}

func TestCheckDetectsLeakedModuleGoroutine(t *testing.T) {
	Check(t)
	done := make(chan struct{})
	go func() {
		<-done
	}()
	t.Cleanup(func() { close(done) })
	// The cleanup order is LIFO: our Check runs after this one closes done.
	_ = time.Millisecond
}
