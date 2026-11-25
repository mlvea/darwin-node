package main

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
)

func TestUnixListenFlagDefaultsOff(t *testing.T) {
	cmd := newCommand()
	f := cmd.PersistentFlags().Lookup("unix-listen")
	if f == nil {
		t.Fatal("unix-listen flag missing")
	}
	if f.DefValue != "" {
		t.Fatalf("unix-listen default %q, want empty", f.DefValue)
	}
	if cmd.PersistentFlags().Lookup("listen") != nil || cmd.Flags().Lookup("listen") != nil {
		t.Fatal("TCP listen flag is back")
	}
	if unixListenPath() != "" && os.Getenv(envUnixListen) == "" {
		t.Fatalf("path %q with empty env", unixListenPath())
	}
}

func TestUnixListenPathTrims(t *testing.T) {
	old := unixListenFlag
	t.Cleanup(func() { unixListenFlag = old })
	unixListenFlag = "  /tmp/x.sock  "
	if unixListenPath() != "/tmp/x.sock" {
		t.Fatalf("path %q", unixListenPath())
	}
	unixListenFlag = ""
	if unixListenPath() != "" {
		t.Fatalf("cleared path %q", unixListenPath())
	}
}

func TestApplyUnixListenIdle(t *testing.T) {
	old := unixIdleFlag
	t.Cleanup(func() { unixIdleFlag = old })
	unixIdleFlag = "40ms"
	h := newHandler("t", false, nil)
	if err := applyUnixListen(h); err != nil {
		t.Fatal(err)
	}
	if h.IdleTimeout != 40*time.Millisecond {
		t.Fatalf("idle %s", h.IdleTimeout)
	}
	unixIdleFlag = ""
	h = newHandler("t", false, nil)
	if err := applyUnixListen(h); err != nil {
		t.Fatal(err)
	}
	if h.IdleTimeout != 0 {
		t.Fatalf("default idle must stay unset, got %s", h.IdleTimeout)
	}
	unixIdleFlag = "nope"
	if err := applyUnixListen(h); err == nil {
		t.Fatal("expected idle-timeout parse error")
	}
}

func TestUnixListenHandshake(t *testing.T) {
	f, err := os.CreateTemp("/tmp", "ga-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(path) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHandler("tok", false, nil)
	h.IdleTimeout = time.Second
	h.Init()
	errc := make(chan error, 1)
	go func() { errc <- listenUnix(ctx, h, path) }()

	var conn net.Conn
	deadline := time.Now().Add(2 * time.Second)
	var last error
	for {
		c, derr := net.Dial("unix", path)
		if derr == nil {
			conn = c
			break
		}
		last = derr
		if time.Now().After(deadline) {
			cancel()
			<-errc
			t.Fatalf("dial %s: %v", path, last)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cli, err := guest.Dial(ctx, conn, "tok", "test")
	if err != nil {
		cancel()
		<-errc
		t.Fatal(err)
	}
	ready, err := cli.Ready(ctx)
	if err != nil || !ready.Ready {
		t.Fatalf("ready %+v %v", ready, err)
	}
	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listen loop did not return")
	}
}

func TestUnixListenIdleClosesSilentConn(t *testing.T) {
	f, err := os.CreateTemp("/tmp", "ga-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	_ = f.Close()
	_ = os.Remove(path)
	t.Cleanup(func() { _ = os.Remove(path) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := newHandler("tok", false, nil)
	h.IdleTimeout = 300 * time.Millisecond
	h.Init()
	errc := make(chan error, 1)
	go func() { errc <- listenUnix(ctx, h, path) }()

	var conn net.Conn
	deadline := time.Now().Add(2 * time.Second)
	for {
		c, derr := net.Dial("unix", path)
		if derr == nil {
			conn = c
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal(derr)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// No handshake. The idle watchdog must close the socket on its own.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 8)
	start := time.Now()
	_, rerr := conn.Read(buf)
	if rerr == nil {
		t.Fatal("silent connection stayed open")
	}
	if time.Since(start) > 1500*time.Millisecond {
		t.Fatalf("idle close took %s", time.Since(start))
	}
	_ = conn.Close()
	cancel()
	select {
	case <-errc:
	case <-time.After(2 * time.Second):
		t.Fatal("listen loop did not return")
	}
}
