package vz

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestAcceptAgentTimeoutClosesListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	c, err := acceptAgent(ctx, ln)
	if err == nil || c != nil {
		t.Fatalf("want timeout, got %v %v", c, err)
	}
	// Listener must be closed so Accept does not leak.
	time.Sleep(20 * time.Millisecond)
	if err := ln.Close(); err == nil {
		t.Fatal("listener should already be closed after timeout")
	}
}

func TestAgentDialContextCapsWhenNoDeadline(t *testing.T) {
	ctx, cancel := agentDialContext(context.Background())
	defer cancel()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline")
	}
	remain := time.Until(dl)
	if remain < 40*time.Second || remain > 46*time.Second {
		t.Fatalf("deadline remaining %v, want ~45s", remain)
	}
}

func TestVsockAttemptUsesFullBudget(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	ctx, cancel2 := vsockAttemptContext(parent)
	defer cancel2()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline")
	}
	parentDL, _ := parent.Deadline()
	gap := parentDL.Sub(dl)
	// TCP fallback is gone. Holding back part of the dial budget left a
	// slow guest with less than AgentReadyTimeout to connect.
	if gap > 50*time.Millisecond {
		t.Fatalf("vsock deadline is %v before parent; the dial budget was reserved for TCP", gap)
	}
}

func TestVsockAttemptUsesShortBudget(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	ctx, cancel2 := vsockAttemptContext(parent)
	defer cancel2()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline")
	}
	parentDL, _ := parent.Deadline()
	gap := parentDL.Sub(dl)
	if gap > 50*time.Millisecond {
		t.Fatalf("vsock deadline is %v before parent; the dial budget was reserved for TCP", gap)
	}
}

func TestAgentDialContextKeepsCallerDeadline(t *testing.T) {
	parent, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	ctx, cancel2 := agentDialContext(parent)
	defer cancel2()
	dl, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected deadline")
	}
	parentDL, _ := parent.Deadline()
	if dl.After(parentDL.Add(time.Millisecond)) {
		t.Fatalf("dial ctx deadline %v later than parent %v", dl, parentDL)
	}
}

func TestAcceptAgentSuccess(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err == nil {
			_ = c.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c, err := acceptAgent(ctx, ln)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}
