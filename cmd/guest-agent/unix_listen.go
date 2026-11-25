package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/guest"

	"github.com/spf13/cobra"
)

// Unix-socket listen is off unless a test or SimVZ asks for it. The agent
// still dials virtio-vsock in the guest. This listener is the host side of
// that stream when there is no Virtualization.framework: SimVZ dials it.
// It is not a TCP listener and it does not replace vsock on a real Mac.
const (
	envUnixListen = "DARWIN_NODE_AGENT_UNIX_LISTEN"
	envUnixIdle   = "DARWIN_NODE_AGENT_IDLE"
)

var (
	unixListenFlag string
	unixIdleFlag   string
)

func registerUnixListen(cmd *cobra.Command) {
	f := cmd.PersistentFlags()
	f.StringVar(&unixListenFlag, "unix-listen", os.Getenv(envUnixListen), "unix socket to accept host connections (default off; not vsock and not TCP)")
	f.StringVar(&unixIdleFlag, "idle-timeout", os.Getenv(envUnixIdle), "guest idle timeout (default 30s; empty keeps the protocol default)")
}

func unixListenPath() string {
	return strings.TrimSpace(unixListenFlag)
}

func applyUnixListen(h *guest.Handler) error {
	if h == nil {
		return nil
	}
	s := strings.TrimSpace(unixIdleFlag)
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("idle-timeout: %w", err)
	}
	if d > 0 {
		h.IdleTimeout = d
	}
	return nil
}

// serveOnUnixSocket accepts host connections until ctx is cancelled.
// A workload still runs under guest.Supervise; the listen loop is the
// control connection, not a second copy of the process.
func serveOnUnixSocket(ctx context.Context, h *guest.Handler, path string, workload []string) error {
	errc := make(chan error, 1)
	go func() {
		errc <- listenUnix(ctx, h, path)
	}()
	if len(workload) > 0 {
		if err := guest.Supervise(ctx, h, workload, guest.DefaultExitSendTimeout, guest.HaltGuest); err != nil {
			fmt.Fprintf(os.Stderr, "workload: %v\n", err)
		}
		<-ctx.Done()
		<-errc
		return nil
	}
	fmt.Fprintf(os.Stderr, "darwin-guest-agent %s unix %s\n", config.Version, path)
	select {
	case <-ctx.Done():
		<-errc
		return nil
	case err := <-errc:
		return err
	}
}

func listenUnix(ctx context.Context, h *guest.Handler, path string) error {
	if path == "" {
		return fmt.Errorf("unix listen path is empty")
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(path) }()
	defer func() { _ = ln.Close() }()

	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()

	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			_ = guest.Serve(ctx, c, *h)
		}(conn)
	}
}
