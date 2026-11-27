package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/types"

	"github.com/spf13/cobra"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := newCommand().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func newCommand() *cobra.Command {
	var (
		token     string
		tokenFile string
		insecure  bool
	)
	cmd := &cobra.Command{
		Use:   "darwin-guest-agent",
		Short: "In-guest agent for darwin-node (exec, logs, probes, metrics)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd, token, tokenFile, insecure, nil)
		},
	}
	// Persistent so `run` accepts the same flags.
	f := cmd.PersistentFlags()
	f.StringVar(&token, "token", os.Getenv("DARWIN_NODE_AGENT_TOKEN"), "shared handshake token")
	f.StringVar(&tokenFile, "token-file", defaultTokenFile(), "token file (virtio-fs control share)")
	f.BoolVar(&insecure, "insecure-no-token", false, "allow empty token (development only)")
	registerUnixListen(cmd)
	cmd.AddCommand(&cobra.Command{
		Use:   "run -- command [args...]",
		Short: "Run the main workload, report its exit code, then halt",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServe(cmd, token, tokenFile, insecure, args)
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

	return cmd
}

func defaultTokenFile() string {
	return types.GuestAutomountRoot + "/" + types.ControlShareName + "/" + types.GuestAgentTokenFile
}

func runServe(cmd *cobra.Command, token, tokenFile string, insecure bool, workload []string) error {
	if token == "" && tokenFile != "" {
		if t, err := guest.ReadTokenFile(tokenFile); err == nil {
			token = t
		}
	}
	return serve(cmd.Context(), token, tokenFile, insecure, workload)
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
		// Darwin writes /dev/random and renames the guest. Other GOOS values
		// get the in-memory platform so this binary can be tested on Linux.
		AdoptPlatform: guest.NewHostPlatform(),
	}
}

func serve(ctx context.Context, token, tokenFile string, insecure bool, workload []string) error {
	// The agent is reached only over vsock. It does not listen on TCP:
	// on the shared NAT the other VM could read the handshake token.
	// With no workload the process serves exec, probes, and logs until
	// it is stopped. `run` supervises the main process and, when it
	// exits, writes an Exit notify before halting. See guest.ExitReport.
	// Shutdown syncs and halts so a pod stop flushes shared volumes.
	// --unix-listen is the SimVZ stand-in and stays off unless set.
	h := newHandler(token, insecure, nil)
	if err := applyUnixListen(h); err != nil {
		return err
	}
	h.Init()
	if tokenFile != "" {
		go h.WatchTokenFile(ctx, tokenFile, 2*time.Second)
	}
	if path := unixListenPath(); path != "" {
		return serveOnUnixSocket(ctx, h, path, workload)
	}
	go dialHostVsock(ctx, h)

	if len(workload) > 0 {
		if err := guest.Supervise(ctx, h, workload, guest.DefaultExitSendTimeout, guest.HaltGuest); err != nil {
			fmt.Fprintf(os.Stderr, "workload: %v\n", err)
		}
		// KeepAlive would start the workload again if this process
		// returned. Block until the VM halt or a signal kills us.
		<-ctx.Done()
		return nil
	}

	fmt.Fprintf(os.Stderr, "darwin-guest-agent %s vsock-only\n", config.Version)
	<-ctx.Done()
	return nil
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
