package vz

import (
	"context"
	"net"
	"time"
)

// defaultAgentDialTimeout caps vsock Accept / TCP fallback when the caller
// did not set a deadline (start() uses an unbounded pod context).
const defaultAgentDialTimeout = 45 * time.Second

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
