package vz

import (
	"context"
	"errors"
	"net"
	"sync"
)

// agentDispatch accepts guest connections and hands one to a dial that is
// waiting. Any other connection is closed.
//
// Code-Hex/vz's virtio-socket listener keeps a one-slot buffer and starts a
// goroutine per later connection that blocks until Accept takes it. That
// goroutine holds the duplicated fd. DialAgent used to Accept only while a
// dial was in progress, so each extra guest connect leaked a goroutine and
// an fd. The loop below Accepts continuously. Hardware confirmation is
// still required; the Linux tests drive the same queue shape with pipes.
type agentDispatch struct {
	ln net.Listener

	mu     sync.Mutex
	waiter chan net.Conn
	closed bool

	done chan struct{}
	once sync.Once
}

func startAgentDispatch(ln net.Listener) *agentDispatch {
	if ln == nil {
		return nil
	}
	d := &agentDispatch{ln: ln, done: make(chan struct{})}
	go d.loop()
	return d
}

func (d *agentDispatch) loop() {
	for {
		c, err := d.ln.Accept()
		if c != nil && !d.offer(c) {
			_ = c.Close()
		}
		if err != nil {
			return
		}
	}
}

func (d *agentDispatch) offer(c net.Conn) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.waiter == nil {
		return false
	}
	select {
	case d.waiter <- c:
		d.waiter = nil
		return true
	default:
		return false
	}
}

// Wait returns the next guest connection. The caller must Close it.
// A timeout does not close the listener: the accept loop keeps draining
// so a guest that reconnects cannot fill the host's fd table.
func (d *agentDispatch) Wait(ctx context.Context) (net.Conn, error) {
	if d == nil {
		return nil, net.ErrClosed
	}
	ch := make(chan net.Conn, 1)
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil, net.ErrClosed
	}
	if d.waiter != nil {
		d.mu.Unlock()
		return nil, errors.New("agent dial already in progress")
	}
	d.waiter = ch
	d.mu.Unlock()

	select {
	case c := <-ch:
		return c, nil
	case <-ctx.Done():
		d.mu.Lock()
		if d.waiter == ch {
			d.waiter = nil
		}
		d.mu.Unlock()
		select {
		case c := <-ch:
			_ = c.Close()
		default:
		}
		return nil, ctx.Err()
	case <-d.done:
		select {
		case c := <-ch:
			_ = c.Close()
		default:
		}
		return nil, net.ErrClosed
	}
}

// Close stops the accept loop and closes the listener. A conn already
// handed to Wait is the caller's. A conn still in the waiter slot is closed.
func (d *agentDispatch) Close() error {
	if d == nil {
		return nil
	}
	var err error
	d.once.Do(func() {
		d.mu.Lock()
		d.closed = true
		w := d.waiter
		d.waiter = nil
		d.mu.Unlock()
		close(d.done)
		if w != nil {
			select {
			case c := <-w:
				_ = c.Close()
			default:
			}
		}
		if d.ln != nil {
			err = d.ln.Close()
		}
	})
	return err
}
