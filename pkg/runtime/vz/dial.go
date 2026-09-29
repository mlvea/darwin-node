package vz

import (
	"context"
	"net"
	"time"
)

// defaultAgentDialTimeout caps vsock Accept / TCP fallback when the caller
// did not set a deadline (start() uses an unbounded pod context).
const defaultAgentDialTimeout = 45 * time.Second

// tcpFallbackReserve is left on the dial deadline so a vsock Accept that
// nobody answers cannot consume the entire budget before TCP is tried.
const tcpFallbackReserve = 5 * time.Second

// vsockAttemptContext returns a child of parent that ends tcpFallbackReserve
// before parent's deadline, when there is room. Shorter budgets are split.
func vsockAttemptContext(parent context.Context) (context.Context, context.CancelFunc) {
	dl, ok := parent.Deadline()
	if !ok {
		return context.WithCancel(parent)
	}
	remain := time.Until(dl)
	reserve := tcpFallbackReserve
	if remain <= reserve {
		if remain <= 0 {
			return context.WithCancel(parent)
		}
		reserve = remain / 2
		if reserve <= 0 {
			return context.WithCancel(parent)
		}
	}
	return context.WithDeadline(parent, dl.Add(-reserve))
}

func agentDialContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, defaultAgentDialTimeout)
}

// acceptAgent waits for the guest to dial the host vsock listener.
// On timeout the listener is closed so Accept unblocks and a late
// connection is discarded (the guest is the dialer; the host never Connects).
func acceptAgent(ctx context.Context, ln net.Listener) (net.Conn, error) {
	if ln == nil {
		return nil, context.Canceled
	}
	type acc struct {
		c   net.Conn
		err error
	}
	ch := make(chan acc, 1)
	go func() {
		c, err := ln.Accept()
		ch <- acc{c, err}
	}()
	select {
	case <-ctx.Done():
		_ = ln.Close()
		go func() {
			a := <-ch
			if a.c != nil {
				_ = a.c.Close()
			}
		}()
		return nil, ctx.Err()
	case a := <-ch:
		if a.err != nil {
			return nil, a.err
		}
		return a.c, nil
	}
}
