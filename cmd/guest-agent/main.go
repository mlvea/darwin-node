package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/types"

	"github.com/spf13/cobra"
)

func main() {
	var (
		listen      string
		token       string
		tokenFile   string
		insecure    bool
		tcpFallback bool
	)
	cmd := &cobra.Command{
		Use:   "darwin-guest-agent",
		Short: "In-guest agent for darwin-node (exec, logs, probes, metrics)",
		RunE: func(cmd *cobra.Command, args []string) error {
			if token == "" && tokenFile != "" {
				if t, err := guest.ReadTokenFile(tokenFile); err == nil {
					token = t
				}
			}
			if listen == "" {
				listen = ":" + strconv.Itoa(types.GuestTCPPort)
			}
			return serve(cmd.Context(), listen, token, tokenFile, insecure, tcpFallback)
		},
	}
	cmd.Flags().StringVar(&listen, "listen", ":"+strconv.Itoa(types.GuestTCPPort), "TCP listen address (used with --agent-tcp-fallback)")
	cmd.Flags().StringVar(&token, "token", os.Getenv("DARWIN_NODE_AGENT_TOKEN"), "shared handshake token")
	cmd.Flags().StringVar(&tokenFile, "token-file", defaultTokenFile(), "token file (virtio-fs control share)")
	cmd.Flags().BoolVar(&insecure, "insecure-no-token", false, "allow empty token (development only)")
	cmd.Flags().BoolVar(&tcpFallback, "agent-tcp-fallback", false, "listen on plaintext TCP in addition to vsock")
	cmd.AddCommand(&cobra.Command{
		Use:   "selftest [name]",
		Short: "Run an in-guest self-test (agent|metal)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := "agent"
			if len(args) > 0 {
				name = args[0]
			}
			res := guest.RunSelftest(name)
			if !res.OK {
				return fmt.Errorf("%s", res.Message)
			}
			fmt.Println(res.Message)
			return nil
		},
	})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func defaultTokenFile() string {
	return types.GuestAutomountRoot + "/" + types.ControlShareName + "/" + types.GuestAgentTokenFile
}

func serve(ctx context.Context, listen, token, tokenFile string, insecure, tcpFallback bool) error {
	h := &guest.Handler{
		Token:                token,
		AllowInsecureNoToken: insecure,
		AgentVersion:         config.Version,
		LogBuffer:            guest.NewLogBuffer(1024),
	}
	h.Init()
	if tokenFile != "" {
		go h.WatchTokenFile(ctx, tokenFile, 2*time.Second)
	}
	go dialHostVsock(ctx, h)

	if !tcpFallback {
		fmt.Fprintf(os.Stderr, "darwin-guest-agent %s vsock-only (TCP fallback disabled)\n", config.Version)
		<-ctx.Done()
		return nil
	}

	if h.CurrentToken() == "" && !insecure {
		fmt.Fprintf(os.Stderr, "waiting for non-empty agent token before TCP listen (%s)\n", tokenFile)
		if err := waitForToken(ctx, h, tokenFile); err != nil {
			return err
		}
	}

	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "darwin-guest-agent %s listening on %s\n", config.Version, listen)
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go func(conn net.Conn) {
			defer conn.Close()
			_ = guest.Serve(ctx, conn, *h)
		}(c)
	}
}

func waitForToken(ctx context.Context, h *guest.Handler, tokenFile string) error {
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if h.CurrentToken() != "" {
			return nil
		}
		if tokenFile != "" {
			if tok, err := guest.ReadTokenFile(tokenFile); err == nil && tok != "" {
				h.SetToken(tok)
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func dialHostVsock(ctx context.Context, h *guest.Handler) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := guest.DialHostVsock(uint32(types.GuestVsockPort))
		if err != nil {
			t := time.NewTimer(backoff)
			select {
			case <-ctx.Done():
				t.Stop()
				return
			case <-t.C:
			}
			if backoff < 30*time.Second {
				backoff *= 2
			}
			continue
		}
		fmt.Fprintf(os.Stderr, "connected to host via vsock port %d\n", types.GuestVsockPort)
		_ = guest.Serve(ctx, conn, *h)
		_ = conn.Close()
		backoff = time.Second
	}
}
