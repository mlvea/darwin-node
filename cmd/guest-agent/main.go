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
			return runServe(cmd, listen, token, tokenFile, insecure, tcpFallback, nil)
		},
	}
	// Persistent so `run` accepts the same flags. The names and defaults
	// are unchanged.
	f := cmd.PersistentFlags()
	f.StringVar(&listen, "listen", ":"+strconv.Itoa(types.GuestTCPPort), "TCP listen address (used with --agent-tcp-fallback)")
	f.StringVar(&token, "token", os.Getenv("DARWIN_NODE_AGENT_TOKEN"), "shared handshake token")
	f.StringVar(&tokenFile, "token-file", defaultTokenFile(), "token file (virtio-fs control share)")
	f.BoolVar(&insecure, "insecure-no-token", false, "allow empty token (development only)")
	f.BoolVar(&tcpFallback, "agent-tcp-fallback", false, "listen on plaintext TCP in addition to vsock")
	cmd.AddCommand(&cobra.Command{
		Use:   "run -- command [args...]",
		Short: "Run the main workload, report its exit code, then halt",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd, listen, token, tokenFile, insecure, tcpFallback, args)
		},
	})
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

func runServe(cmd *cobra.Command, listen, token, tokenFile string, insecure, tcpFallback bool, workload []string) error {
	if token == "" && tokenFile != "" {
		if t, err := guest.ReadTokenFile(tokenFile); err == nil {
			token = t
		}
	}
	if listen == "" {
		listen = ":" + strconv.Itoa(types.GuestTCPPort)
	}
	return serve(cmd.Context(), listen, token, tokenFile, insecure, tcpFallback, workload)
}

func newHandler(token string, insecure bool, stop guest.Stopper) *guest.Handler {
	if stop == nil {
		stop = guest.OSStopper{}
	}
	return &guest.Handler{
		Token:                token,
		AllowInsecureNoToken: insecure,
		AgentVersion:         config.Version,
		LogBuffer:            guest.NewLogBuffer(1024),
		ShutdownFn:           guest.ShutdownFunc(stop),
	}
}

func serve(ctx context.Context, listen, token, tokenFile string, insecure, tcpFallback bool, workload []string) error {
	// With no workload the agent serves exec, probes, and logs until the
	// process is stopped. `run` supervises the main process and, when it
	// exits, writes an Exit notify before halting. See guest.ExitReport.
	// Shutdown syncs and halts so a pod stop flushes shared volumes.
	h := newHandler(token, insecure, nil)
	h.Init()
	if tokenFile != "" {
		go h.WatchTokenFile(ctx, tokenFile, 2*time.Second)
	}
	go dialHostVsock(ctx, h)

	if len(workload) > 0 {
		if tcpFallback {
			go func() { _ = listenTCP(ctx, h, listen, tokenFile, insecure) }()
		}
		if err := guest.Supervise(ctx, h, workload, guest.DefaultExitSendTimeout, guest.HaltGuest); err != nil {
			fmt.Fprintf(os.Stderr, "workload: %v\n", err)
		}
		// KeepAlive would start the workload again if this process
		// returned. Block until the VM halt or a signal kills us.
		<-ctx.Done()
		return nil
	}

	if !tcpFallback {
		fmt.Fprintf(os.Stderr, "darwin-guest-agent %s vsock-only (TCP fallback disabled)\n", config.Version)
		<-ctx.Done()
		return nil
	}
	return listenTCP(ctx, h, listen, tokenFile, insecure)
}

func listenTCP(ctx context.Context, h *guest.Handler, listen, tokenFile string, insecure bool) error {
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
