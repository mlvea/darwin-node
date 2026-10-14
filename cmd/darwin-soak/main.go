// darwin-soak runs the adopt/delete soak from pkg/soak.
//
//	make soak
//
// cycles cache-volume pods through the fake runtime (no
// Virtualization.framework). On Apple Silicon, a baked image selects the
// vz runtime and a 24 hour budget:
//
//	make soak IMAGE=out/macos-15
//
// The process is capped at 2 concurrent VMs. A third create while both
// slots are held must be refused, and the run fails if file descriptors,
// pod directories, or slot occupancy drift.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	goruntime "runtime"
	"syscall"
	"time"

	dnconfig "github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	dnruntime "github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	vzrt "github.com/darwin-node/darwin-node/pkg/runtime/vz"
	"github.com/darwin-node/darwin-node/pkg/soak"
	"github.com/darwin-node/darwin-node/pkg/types"

	"github.com/spf13/cobra"
)

func main() {
	var (
		image    string
		cacheDir string
		cycles   int
		duration time.Duration
		maxVMs   int
		width    int
		boot     time.Duration
		grace    int64
		fdSlack  int
		keep     bool
	)
	root := &cobra.Command{
		Use:           "darwin-soak",
		Short:         "Adopt/delete soak with cache volumes (at most 2 VMs)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSoak(image, cacheDir, cycles, duration, maxVMs, width, boot, grace, fdSlack, keep)
		},
	}
	flags := root.Flags()
	flags.StringVar(&image, "image", "", "baked macOS image directory; empty uses the fake runtime")
	flags.StringVar(&cacheDir, "cache-dir", "", "node cache directory (default: a fresh temp dir)")
	flags.IntVar(&cycles, "cycles", 0, "stop after this many adopt/delete cycles (default: 4 for fake, unlimited when --image sets a 24h budget)")
	flags.DurationVar(&duration, "duration", 0, "wall budget checked between cycles (default: 24h when --image is set)")
	flags.IntVar(&maxVMs, "max-vms", types.AppleMaxConcurrentVMs, "VM slot cap, 1 or 2")
	flags.IntVar(&width, "width", 0, "pods per cycle, 1..max-vms (default: fill every slot)")
	flags.DurationVar(&boot, "boot-timeout", 0, "wait for PodRunning (default: 30s fake, 10m with --image)")
	flags.Int64Var(&grace, "delete-grace", -1, "delete grace seconds (-1: 0 for fake, 20 with --image)")
	flags.IntVar(&fdSlack, "fd-slack", 0, "allowed open-fd growth over the pre-cycle baseline (0 uses 8)")
	flags.BoolVar(&keep, "keep", false, "keep the cache directory after the run")
	if err := root.Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "SOAK FAIL %v\n", err)
		os.Exit(1)
	}
}

func runSoak(image, cacheDir string, cycles int, duration time.Duration, maxVMs, width int, boot time.Duration, grace int64, fdSlack int, keep bool) error {
	if cycles == 0 && duration == 0 {
		if image == "" {
			cycles = 4
		} else {
			duration = 24 * time.Hour
		}
	}
	if boot == 0 {
		boot = 30 * time.Second
		if image != "" {
			boot = 10 * time.Minute
		}
	}
	if grace < 0 {
		grace = 0
		if image != "" {
			grace = 20
		}
	}

	ownCache := false
	if cacheDir == "" {
		dir, err := os.MkdirTemp("", "darwin-soak-")
		if err != nil {
			return err
		}
		cacheDir = dir
		ownCache = true
	}
	if ownCache && !keep {
		defer func() { _ = os.RemoveAll(cacheDir) }()
	}

	cfg := soak.Config{
		CacheDir:    cacheDir,
		Image:       image,
		MaxVMs:      maxVMs,
		Width:       width,
		Cycles:      cycles,
		Duration:    duration,
		BootTimeout: boot,
		Grace:       grace,
		FDSlack:     fdSlack,
		Events:      event.Nop{},
		Logf: func(format string, args ...any) {
			fmt.Printf(format+"\n", args...)
		},
	}
	if image == "" {
		fr := fake.New()
		cfg.Runtime = fr
		cfg.Running = fr.Running
		cfg.AgentReadyTimeout = 5 * time.Second
	} else {
		if goruntime.GOOS != "darwin" || goruntime.GOARCH != "arm64" {
			return fmt.Errorf("a baked image requires darwin/arm64 (Virtualization.framework); this host is %s/%s. Omit --image for the fake-runtime soak", goruntime.GOOS, goruntime.GOARCH)
		}
		st, err := os.Stat(image)
		if err != nil || !st.IsDir() {
			return fmt.Errorf("--image %q is not a directory", image)
		}
		cfg.Runtime = vzrt.New(dnruntime.Options{
			CacheDir:     cacheDir,
			NetworkMode:  types.NetworkNAT,
			HostAgentVer: dnconfig.Version,
		})
		cfg.AgentReadyTimeout = 5 * time.Minute
		cfg.Events = event.Slog{}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, err := soak.Run(ctx, cfg)
	if keep || !ownCache {
		fmt.Printf("SOAK cache %s\n", cacheDir)
	}
	return err
}
