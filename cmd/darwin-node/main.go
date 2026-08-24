package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	dnconfig "github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/debug"
	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/node"
	"github.com/darwin-node/darwin-node/pkg/observability"
	"github.com/darwin-node/darwin-node/pkg/provider"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	vzrt "github.com/darwin-node/darwin-node/pkg/runtime/vz"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	"github.com/spf13/cobra"
	vknode "github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

func main() {
	cfg := dnconfig.Default()
	var standalone bool

	cmd := &cobra.Command{
		Use:   "darwin-node",
		Short: "Apple Silicon Kubernetes node agent for native macOS VMs",
		Long:  "darwin-node presents this Mac as a Kubernetes node and runs at most two macOS VMs as pods.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cfg.ApplyEnv(); err != nil {
				return err
			}
			if err := cfg.Validate(); err != nil {
				return err
			}
			observability.SetupSlog(cfg.LogLevel)
			return run(cmd.Context(), cfg, standalone)
		},
	}
	f := cmd.Flags()
	f.StringVar(&cfg.NodeName, "nodename", cfg.NodeName, "Kubernetes node name")
	f.StringVar(&cfg.ProviderID, "provider-id", cfg.ProviderID, "provider ID reported to the API server")
	f.StringVar((*string)(&cfg.Runtime), "runtime", string(cfg.Runtime), "vz (hardware) or fake (tests/CI)")
	f.IntVar(&cfg.MaxVMs, "max-vms", cfg.MaxVMs, "max concurrent macOS VMs (1 or 2)")
	f.StringVar((*string)(&cfg.NetworkMode), "network-mode", string(cfg.NetworkMode), "nat or bridged")
	f.StringVar(&cfg.BridgeInterface, "bridge-interface", cfg.BridgeInterface, "host interface for bridged mode")
	f.BoolVar(&cfg.AllowNATWorkloads, "allow-nat-workloads", cfg.AllowNATWorkloads, "do not taint NAT-only nodes")
	f.BoolVar(&cfg.DisableTaint, "disable-taint", cfg.DisableTaint, "do not taint the node")
	f.StringVar(&cfg.CacheDir, "cache-dir", cfg.CacheDir, "image and overlay cache")
	f.StringSliceVar(&cfg.AllowedHostPaths, "allowed-host-paths", cfg.AllowedHostPaths, "hostPath prefixes permitted in guests (empty denies all hostPath)")
	f.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "debug|info|warn|error")
	f.StringVar(&cfg.KubeConfig, "kubeconfig", cfg.KubeConfig, "kubeconfig path")
	f.StringVar(&cfg.ListenAddress, "listen-address", cfg.ListenAddress, "kubelet HTTP bind address (empty = all interfaces)")
	f.BoolVar(&standalone, "standalone", false, "run engine without joining a cluster (dev)")
	f.DurationVar(&cfg.StartupTimeout, "startup-timeout", cfg.StartupTimeout, "wait for node Ready")
	f.DurationVar(&cfg.InitTimeout, "init-timeout", cfg.InitTimeout, "max time to wait for each init container")
	f.StringSliceVar(&cfg.InsecureRegistries, "insecure-registry", cfg.InsecureRegistries, "registries allowed to use plaintext HTTP (loopback is always allowed)")

	cmd.AddCommand(cmdDebugDump(&cfg))

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg dnconfig.Config, standalone bool) error {
	if err := dnconfig.RequireKubeletTLS(standalone, cfg); err != nil {
		return err
	}
	slots, err := capacity.New(cfg.MaxVMs)
	if err != nil {
		return err
	}
	rt, err := newRuntime(cfg)
	if err != nil {
		return err
	}
	host, herr := node.ProbeHost(ctx)
	if herr != nil && cfg.Runtime != types.RuntimeFake {
		return fmt.Errorf("probe host: %w", herr)
	}
	if host.LogicalCPUs == 0 {
		host.LogicalCPUs = 8
		host.MemoryBytes = 16 << 30
		host.DiskBytes = 100 << 30
		host.Arch = "arm64"
	}

	sc := sidecar.Runtime(sidecar.None{})
	if cfg.Runtime == types.RuntimeFake {
		sc = sidecar.NewMemory()
	} else if d, err := sidecar.NewDocker(ctx); err == nil {
		sc = d
	}
	if cfg.OTELEndpoint != "" {
		shutdown, err := observability.StartOTLP(ctx, cfg.OTELEndpoint)
		observability.LogStartError(func(msg string, args ...any) { slog.Error(msg, args...) }, err, cfg.OTELEndpoint)
		if err == nil {
			defer func() { _ = shutdown(context.Background()) }()
		}
	}
	eng := engine.New(cfg, slots, rt, sc, event.Slog{}, host.InternalIP)
	inv := node.Inventory{Host: host, Cfg: cfg, Slots: slots}
	p := provider.New(cfg, eng, inv, event.Slog{})
	metrics := observability.NewMetrics()
	metrics.Observe(func() (float64, float64) { return float64(slots.Used()), float64(len(eng.List())) })
	go func() { _ = observability.ServeMetrics("127.0.0.1:9100", metrics.Handler()) }()

	if standalone {
		fmt.Printf("darwin-node standalone runtime=%s maxVMs=%d node=%s\n", cfg.Runtime, cfg.MaxVMs, cfg.NodeName)
		n := &corev1.Node{}
		if err := p.ConfigureNode(ctx, n); err != nil {
			return err
		}
		podsQ := n.Status.Capacity[corev1.ResourcePods]
		vmQ := n.Status.Capacity[corev1.ResourceName(types.ResourceVM)]
		cpuQ := n.Status.Capacity[corev1.ResourceCPU]
		memQ := n.Status.Capacity[corev1.ResourceMemory]
		fmt.Printf("capacity pods=%s vm=%s cpu=%s memory=%s\n",
			podsQ.String(), vmQ.String(), cpuQ.String(), memQ.String())
		debugDir := filepath.Join(cfg.CacheDir, "debug")
		_ = debug.Capture(cfg, inv, eng).Write(filepath.Join(debugDir, "debug-snapshot.json"))
		<-ctx.Done()
		return nil
	}

	k8s, err := nodeutil.ClientsetFromEnv(cfg.KubeConfig)
	if err != nil {
		return fmt.Errorf("kube client: %w (set KUBECONFIG or pass --standalone)", err)
	}
	return runVK(ctx, cfg, k8s, p)
}

func newRuntime(cfg dnconfig.Config) (runtime.Runtime, error) {
	switch cfg.Runtime {
	case types.RuntimeFake:
		return fake.New(), nil
	default:
		return vzrt.New(runtime.Options{
			CacheDir:     cfg.CacheDir,
			NetworkMode:  cfg.NetworkMode,
			BridgeDevice: cfg.BridgeInterface,
			Graphics:     cfg.Graphics,
			HostAgentVer: dnconfig.Version,
		}), nil
	}
}

func runVK(ctx context.Context, cfg dnconfig.Config, k8s kubernetes.Interface, p *provider.Provider) error {
	tlsCfg, err := dnconfig.TLSConfig(cfg.CertPath, cfg.KeyPath, cfg.ClientCA)
	if err != nil {
		return fmt.Errorf("kubelet tls: %w", err)
	}
	n, err := nodeutil.NewNode(cfg.NodeName,
		func(pc nodeutil.ProviderConfig) (nodeutil.Provider, vknode.NodeProvider, error) {
			if err := p.ConfigureNode(ctx, pc.Node); err != nil {
				return nil, nil, err
			}
			p.SetCredentialResolver(provider.KubeResolver{Client: k8s})
			np := node.NewStatusProvider(p.Inventory())
			return p, np, nil
		},
		func(nc *nodeutil.NodeConfig) error {
			return nodeutil.WithClient(k8s)(nc)
		},
		func(nc *nodeutil.NodeConfig) error {
			nc.HTTPListenAddr = cfg.HTTPListenAddr()
			nc.InformerResyncPeriod = cfg.Resync
			nc.NumWorkers = cfg.Workers
			if cfg.ProviderID != "" {
				nc.NodeSpec.Spec.ProviderID = cfg.ProviderID
			}
			return nil
		},
		// TODO(S003): TokenReview + SubjectAccessReview (nodeutil.WebhookAuth +
		// nodeutil.WithAuth) is not wired. WebhookAuth without a client-certificate
		// CA provider would 401 the API server's mTLS kubelet client. Production
		// authn is TLS client certificates when ClientCA is set.
		nodeutil.WithTLSConfig(func(c *tls.Config) error {
			c.Certificates = tlsCfg.Certificates
			c.ClientCAs = tlsCfg.ClientCAs
			c.ClientAuth = tlsCfg.ClientAuth
			return nil
		}),
	)
	if err != nil {
		return err
	}
	go func() { _ = n.Run(ctx) }()
	if err := n.WaitReady(ctx, cfg.StartupTimeout); err != nil {
		return err
	}
	<-n.Done()
	return n.Err()
}

func cmdDebugDump(cfg *dnconfig.Config) *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:   "debug-dump",
		Short: "Write a JSON snapshot of node/slot/pod state for web/debug.html",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cfg.ApplyEnv(); err != nil {
				return err
			}
			cfg.Runtime = types.RuntimeFake
			cfg.AllowNATWorkloads = true
			if cfg.NodeName == "" {
				cfg.NodeName = "debug-mac"
			}
			slots, err := capacity.New(cfg.MaxVMs)
			if err != nil {
				return err
			}
			eng := engine.New(*cfg, slots, fake.New(), sidecar.NewMemory(), event.Nop{}, "127.0.0.1")
			inv := node.Inventory{
				Host:  node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30, Arch: "arm64", InternalIP: "127.0.0.1"},
				Cfg:   *cfg,
				Slots: slots,
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "default", UID: k8stypes.UID("debug-sample")},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "local/macos:test"}}},
			}
			if err := eng.Create(cmd.Context(), pod, engine.Credentials{}); err != nil {
				return err
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if p, err := eng.Get("default", "sample"); err == nil && p.Status.Phase == corev1.PodRunning {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			snap := debug.Capture(*cfg, inv, eng)
			if out == "" {
				out = "debug-snapshot.json"
			}
			if err := snap.Write(out); err != nil {
				return err
			}
			fmt.Println(out)
			return nil
		},
	}
	c.Flags().StringVarP(&out, "out", "o", "debug-snapshot.json", "JSON snapshot path (also writes snapshot.js beside it)")
	c.Flags().StringVar(&cfg.NodeName, "nodename", cfg.NodeName, "node name recorded in the snapshot")
	return c
}
