package vz

import (
	"context"
	"net"
	"time"
)

// defaultAgentDialTimeout caps vsock Accept when the caller did not set
// a deadline (start() uses an unbounded pod context).
const defaultAgentDialTimeout = 45 * time.Second

// vsockAttemptContext is the vsock accept budget. The guest agent is
// reached only over vsock, so the attempt uses the caller's full
// deadline. There is no second transport to reserve time for.
func vsockAttemptContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithCancel(parent)
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
