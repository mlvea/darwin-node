package simvz

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// faultConn is the host end of a vsock stream. The stall holds the first
// write, so the handshake never reaches the guest. The guest idle
// watchdog is armed at accept, before any frame, and closes the socket
// while that write is parked. Delaying the host read instead would leave
// the guest's reply in the socket buffer, and Dial would succeed after
// the sleep. Mid-frame EOF is applied to reads.
type faultConn struct {
	net.Conn
	clock    Clock
	ctx      context.Context
	dial     context.Context
	stall    time.Duration
	eofAfter int

	once sync.Once
	used atomic.Int64
}

func (c *faultConn) Write(p []byte) (int, error) {
	if c == nil {
		return 0, io.ErrClosedPipe
	}
	c.once.Do(func() {
		if c.stall <= 0 || c.clock == nil {
			return
		}
		ctx, cancel := mergeCancel(c.ctx, c.dial)
		defer cancel()
		_ = c.clock.Sleep(ctx, c.stall)
	})
	if c.stall > 0 {
		if c.ctx != nil && c.ctx.Err() != nil {
			return 0, c.ctx.Err()
		}
		if c.dial != nil && c.dial.Err() != nil {
			return 0, c.dial.Err()
		}
	}
	return c.Conn.Write(p)
}

func (c *faultConn) Read(p []byte) (int, error) {
	if c == nil {
		return 0, io.EOF
	}
	if c.eofAfter > 0 {
		used := int(c.used.Load())
		if used >= c.eofAfter {
			_ = c.Close()
			return 0, io.EOF
		}
		if len(p) > c.eofAfter-used {
			p = p[:c.eofAfter-used]
		}
	}
	n, err := c.Conn.Read(p)
	if n > 0 && c.eofAfter > 0 && int(c.used.Add(int64(n))) >= c.eofAfter {
		_ = c.Close()
		if err == nil {
			err = io.EOF
		}
	}
	return n, err
}

// mergeCancel returns a context that ends when either parent ends. The
// goroutine exits when cancel runs, which the caller defers around Sleep,
// so a dial timeout does not leave the stall parked on the VM lifetime.
func mergeCancel(life, dial context.Context) (context.Context, context.CancelFunc) {
	if life == nil {
		life = context.Background()
	}
	ctx, cancel := context.WithCancel(life)
	if dial == nil {
		return ctx, cancel
	}
	go func() {
		select {
		case <-dial.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}
