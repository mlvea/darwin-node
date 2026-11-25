package simvz

import (
	"runtime"
	"sync"
	"sync/atomic"
)

// vmQueue is one actor goroutine per VM, the stand-in for the
// virtualization dispatch queue. Callbacks and completion handlers run
// on it. A synchronous VM call from that goroutine panics: a real
// dispatch_sync back onto the same queue deadlocks, and production code
// must not do it.
type vmQueue struct {
	mu      sync.Mutex
	ch      chan func()
	ready   chan struct{}
	exited  chan struct{}
	stopped bool
	gid     atomic.Uint64
}

func (q *vmQueue) start() {
	q.mu.Lock()
	if q.ch != nil && !q.stopped {
		ready := q.ready
		q.mu.Unlock()
		if ready != nil {
			<-ready
		}
		return
	}
	q.ch = make(chan func())
	q.stopped = false
	q.ready = make(chan struct{})
	q.exited = make(chan struct{})
	ready := q.ready
	go q.loop()
	q.mu.Unlock()
	<-ready
}

func (q *vmQueue) loop() {
	q.gid.Store(goroutineID())
	q.mu.Lock()
	ready := q.ready
	ch := q.ch
	exited := q.exited
	q.mu.Unlock()
	close(ready)
	for fn := range ch {
		fn()
	}
	q.gid.Store(0)
	close(exited)
}

// Do runs fn on the actor. It panics if called from the actor itself.
func (q *vmQueue) Do(fn func()) {
	if fn == nil {
		return
	}
	q.start()
	if q.onActor() {
		panic("simvz: synchronous re-entry on VM queue")
	}
	done := make(chan any, 1)
	q.mu.Lock()
	if q.stopped || q.ch == nil {
		q.mu.Unlock()
		panic("simvz: VM queue is stopped")
	}
	ch := q.ch
	q.mu.Unlock()
	ch <- func() {
		defer func() {
			if r := recover(); r != nil {
				done <- r
				return
			}
			done <- nil
		}()
		fn()
	}
	if r := <-done; r != nil {
		panic(r)
	}
}

func (q *vmQueue) onActor() bool {
	gid := q.gid.Load()
	return gid != 0 && goroutineID() == gid
}

func (q *vmQueue) stop() {
	q.mu.Lock()
	if q.ch == nil || q.stopped {
		q.mu.Unlock()
		return
	}
	q.stopped = true
	ch := q.ch
	exited := q.exited
	q.mu.Unlock()
	close(ch)
	<-exited
}

func (m *machine) mustOutside() {
	if m == nil {
		return
	}
	if m.q.onActor() {
		panic("simvz: synchronous re-entry on VM queue")
	}
}

// OnQueue runs fn on this VM's actor. A synchronous call back into the
// VM from fn panics.
func (m *machine) OnQueue(fn func()) {
	if m == nil {
		return
	}
	m.q.Do(fn)
}

func goroutineID() uint64 {
	var buf [64]byte
	n := runtime.Stack(buf[:], false)
	var id uint64
	for _, b := range buf[:n] {
		if b >= '0' && b <= '9' {
			id = id*10 + uint64(b-'0')
		} else if id != 0 {
			break
		}
	}
	return id
}
