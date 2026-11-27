package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	dnconfig "github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/observability"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"
)

// starter is the process body RunE calls after the config is resolved.
// Tests pass a function that records the config and returns.
type starter func(ctx context.Context, cfg dnconfig.Config, standalone bool, gate *shutdownGate) error

func main() {
	cmd, _, gate, err := buildCommand(run)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// A second signal skips the rest of the drain. Close still runs
	// before exit 130; Graceful blocks in that case so main cannot
	// return 0 first. os.Exit does not return, so Release is unused here.
	go watchSecondSignal(ctx, func() <-chan os.Signal {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		return sig
	}, os.Exit, gate.Force)
	if err := cmd.ExecuteContext(ctx); err != nil {
		if gate.Forced() {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if gate.Forced() {
		os.Exit(130)
	}
}

func buildCommand(start starter) (*cobra.Command, *dnconfig.Config, *shutdownGate, error) {
	cfg := dnconfig.Default()
	// pflag writes each flag's default into the struct when the flag is
	// registered. MergeEnv first so an omitted flag keeps the environment
	// value, and an explicit flag replaces it. RunE must not call ApplyEnv.
	if err := cfg.MergeEnv(); err != nil {
		return nil, nil, nil, err
	}
	sup := dnconfig.DefaultSupervisor()
	if err := sup.MergeEnv(); err != nil {
		return nil, nil, nil, err
	}
	var standalone bool
	gate := newShutdownGate()

	cmd := &cobra.Command{
		Use:   "darwin-node",
		Short: "Apple Silicon Kubernetes node agent for native macOS VMs",
		Long:  "darwin-node presents this Mac as a Kubernetes node and runs at most two macOS VMs as pods.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cfg.Validate(); err != nil {
				return err
			}
			if err := sup.Validate(); err != nil {
				return err
			}
			observability.SetupSlog(cfg.LogLevel)
			ctx := dnconfig.WithSupervisor(cmd.Context(), sup)
			if sup.Role == dnconfig.RoleSupervisor {
				return runSupervisor(ctx, cfg, sup)
			}
			return start(ctx, cfg, standalone, gate)
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfg.NodeName, "nodename", cfg.NodeName, "Kubernetes node name")
	f.StringVar(&cfg.ProviderID, "provider-id", cfg.ProviderID, "provider ID reported to the API server")
	f.StringVar((*string)(&cfg.Runtime), "runtime", string(cfg.Runtime), "vz (hardware) or fake (tests/CI)")
	f.IntVar(&cfg.MaxVMs, "max-vms", cfg.MaxVMs, "max concurrent macOS VMs (1 or 2)")
	f.Var(quantityValue{q: &cfg.ReservedCPU}, "reserved-cpu", "host CPUs withheld from Allocatable")
	f.Var(quantityValue{q: &cfg.ReservedMemory}, "reserved-memory", "host memory withheld from Allocatable")
	f.Var(quantityValue{q: &cfg.ReservedEphemeral}, "reserved-ephemeral", "host ephemeral-storage withheld from Allocatable")
	f.IntVar(&cfg.WarmSlots, "warm-slots", cfg.WarmSlots, "pre-booted idle VMs kept in free slots (0-2); matching pods adopt them instead of cold-booting")
	f.StringVar(&cfg.WarmImage, "warm-image", cfg.WarmImage, "image pre-booted into warm slots (default: most recently used image)")
	f.BoolVar(&cfg.SerialConsole, "serial-console", cfg.SerialConsole, "attach a VM serial port for `darwin-node console` break-glass access")
	f.DurationVar(&cfg.ShutdownGrace, "shutdown-grace-period", cfg.ShutdownGrace, "total budget for graceful pod drain on SIGTERM/SIGINT")
	f.StringVar((*string)(&cfg.NetworkMode), "network-mode", string(cfg.NetworkMode), "nat or bridged")
	f.StringVar(&cfg.BridgeInterface, "bridge-interface", cfg.BridgeInterface, "host interface for bridged mode")
	f.BoolVar(&cfg.AllowNATWorkloads, "allow-nat-workloads", cfg.AllowNATWorkloads, "do not taint NAT-only nodes")
	f.BoolVar(&cfg.DisableTaint, "disable-taint", cfg.DisableTaint, "do not taint the node")
	f.StringVar(&cfg.CacheDir, "cache-dir", cfg.CacheDir, "image and overlay cache")
	f.StringSliceVar(&cfg.CacheWriters, "cache-writers", cfg.CacheWriters, "namespaces or namespace/serviceaccounts allowed to write named caches back (empty: nobody)")
	f.StringSliceVar(&cfg.AllowedHostPaths, "allowed-host-paths", cfg.AllowedHostPaths, "hostPath prefixes permitted in guests (empty denies all hostPath)")
	f.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "debug|info|warn|error")
	f.StringVar(&cfg.KubeConfig, "kubeconfig", cfg.KubeConfig, "kubeconfig path")
	f.BoolVar(&cfg.RotateServerCertificates, "rotate-server-certificates", cfg.RotateServerCertificates, "request kubelet serving certificates (signer kubernetes.io/kubelet-serving). CSRs are not auto-approved")
	f.BoolVar(&cfg.RotateClientCertificates, "rotate-client-certificates", cfg.RotateClientCertificates, "bootstrap and rotate the node client certificate (signer kubernetes.io/kube-apiserver-client-kubelet). Requires --bootstrap-kubeconfig")
	f.StringVar(&cfg.BootstrapKubeconfig, "bootstrap-kubeconfig", cfg.BootstrapKubeconfig, "kubeconfig used to create the first node client CSR")
	f.StringVar(&cfg.CertDir, "cert-dir", cfg.CertDir, "directory for rotated certificates (mode 0700, kubelet-*-current.pem)")
	f.StringVar(&cfg.ListenAddress, "listen-address", cfg.ListenAddress, "kubelet HTTP bind address (default 127.0.0.1; 0.0.0.0 binds every interface)")
	f.BoolVar(&standalone, "standalone", false, "run engine without joining a cluster (dev)")
	f.DurationVar(&cfg.StartupTimeout, "startup-timeout", cfg.StartupTimeout, "wait for node Ready")
	f.DurationVar(&cfg.InitTimeout, "init-timeout", cfg.InitTimeout, "max time to wait for each init container")
	f.DurationVar(&cfg.AgentReadyTimeout, "agent-ready-timeout", cfg.AgentReadyTimeout, "budget for each guest-agent dial and ready check")
	f.StringSliceVar(&cfg.InsecureRegistries, "insecure-registry", cfg.InsecureRegistries, "registries allowed to use plaintext HTTP (loopback is always allowed)")
	f.Int64Var(&cfg.MaxImageBytes, "max-image-bytes", cfg.MaxImageBytes, "max declared or decompressed image bytes (default 256GiB)")
	f.Int64Var(&cfg.MaxImageCompressedBytes, "max-image-compressed-bytes", cfg.MaxImageCompressedBytes, "max compressed blob bytes accepted from a registry (default 256GiB)")
	f.Int64Var(&cfg.ImageDiskHeadroom, "image-disk-headroom", cfg.ImageDiskHeadroom, "free bytes that must remain on the cache filesystem after an image write (default 10GiB)")
	f.DurationVar(&cfg.ImagePullTimeout, "image-pull-timeout", cfg.ImagePullTimeout, "deadline for one image pull; the pull is cancelled when no pod is still waiting")
	f.DurationVar(&cfg.ImageReadStall, "image-read-stall", cfg.ImageReadStall, "fail an image pull when a registry read makes no progress for this long")
	f.BoolVar(&cfg.ImageGC, "image-gc", cfg.ImageGC, "delete unused images and cache stores when the cache disk is above the high threshold")
	f.IntVar(&cfg.ImageGCHigh, "image-gc-high-threshold", cfg.ImageGCHigh, "percent used at which image GC starts (kubelet default 85)")
	f.IntVar(&cfg.ImageGCLow, "image-gc-low-threshold", cfg.ImageGCLow, "percent used at which image GC stops (kubelet default 80)")
	f.DurationVar(&cfg.ImageGCInterval, "image-gc-interval", cfg.ImageGCInterval, "how often to evaluate image and cache-store GC")
	f.StringVar(&cfg.MetricsAddress, "metrics-address", cfg.MetricsAddress, "Prometheus listen address (default 127.0.0.1:9102, not node_exporter's 9100); off disables it")
	registerSupervisorFlags(cmd, &sup)

	cmd.AddCommand(cmdDebugDump(&cfg))
	cmd.AddCommand(cmdConsole(&cfg))
	return cmd, &cfg, gate, nil
}

// quantityValue parses a Kubernetes quantity into a config field.
// The pointer is copied with the value, so Set still updates cfg.
type quantityValue struct{ q *resource.Quantity }

func (v quantityValue) String() string {
	if v.q == nil {
		return "0"
	}
	return v.q.String()
}

func (v quantityValue) Set(s string) error {
	q, err := resource.ParseQuantity(strings.TrimSpace(s))
	if err != nil {
		return err
	}
	if q.Sign() < 0 {
		return fmt.Errorf("negative quantity %q", s)
	}
	*v.q = q
	return nil
}

func (v quantityValue) Type() string { return "quantity" }
