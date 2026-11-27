package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/mlvea/darwin-node/pkg/capacity"
	dnconfig "github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/debug"
	"github.com/mlvea/darwin-node/pkg/engine"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/image"
	"github.com/mlvea/darwin-node/pkg/node"
	"github.com/mlvea/darwin-node/pkg/observability"
	"github.com/mlvea/darwin-node/pkg/provider"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	vzrt "github.com/mlvea/darwin-node/pkg/runtime/vz"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/types"

	vknode "github.com/virtual-kubelet/virtual-kubelet/node"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
)

func run(ctx context.Context, cfg dnconfig.Config, standalone bool, gate *shutdownGate) error {
	if err := rotationWithStandalone(standalone, cfg); err != nil {
		return err
	}
	if err := dnconfig.RequireKubeletTLS(standalone, cfg); err != nil {
		return err
	}
	if err := dnconfig.RequireKubeletClientCA(standalone, cfg.ClientCA); err != nil {
		return err
	}
	slots, err := capacity.New(cfg.MaxVMs)
	if err != nil {
		return err
	}
	rt, err := runtimeFor(ctx, cfg)
	if err != nil {
		return err
	}
	metrics := observability.NewMetrics()
	rt = observability.Meter(rt, metrics)
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
	// A killed process leaves its break-glass sockets in the temp dir.
	// Sweep before any pod listens. Unit tests do not call this with
	// os.TempDir, so they do not delete each other's sockets.
	if err := engine.SweepConsoleSockets(os.TempDir()); err != nil {
		slog.Warn("stale console sockets", "err", err)
	}
	if !standalone {
		cfg.Cluster = true
	}
	eng := engine.New(cfg, slots, rt, sc, event.Slog{}, host.InternalIP)
	gate.bind(eng)
	defer eng.Close()
	inv := node.Inventory{Host: host, Cfg: cfg, Slots: slots}
	p := provider.New(cfg, eng, inv, event.Slog{})
	metrics.Observe(func() (float64, float64) { return float64(slots.Used()), float64(len(eng.List())) })
	image.SetPullObserver(func(d time.Duration, err error) { metrics.ObservePull(d, err) })
	engine.SetProbeObserver(func(kind string, d time.Duration, err error) { metrics.ObserveProbe(kind, d, err) })
	if !dnconfig.MetricsDisabled(cfg.MetricsAddress) {
		stop, err := observability.ListenMetrics(cfg.MetricsAddress, metrics.Handler())
		if err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
		defer func() { _ = stop(context.Background()) }()
	}
	eng.StartDiskGC(ctx)

	if standalone {
		fmt.Printf("darwin-node standalone runtime=%s maxVMs=%d node=%s\n", cfg.Runtime, cfg.MaxVMs, cfg.NodeName)
		n := &corev1.Node{}
		if err := p.ConfigureNode(ctx, n); err != nil {
			return err
		}
		if skew := eng.SupervisorSkew(); skew != nil {
			skew.Apply(n)
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
		gate.Graceful(cfg.ShutdownGrace)
		return nil
	}

	k8s, serving, stopCerts, err := clusterClient(ctx, cfg, host.InternalIP)
	defer stopCerts()
	if err != nil {
		return err
	}
	if err := runVK(ctx, cfg, k8s, p, serving, eng.SupervisorSkew()); err != nil {
		gate.Graceful(cfg.ShutdownGrace)
		return err
	}
	gate.Graceful(cfg.ShutdownGrace)
	return nil
}

// configureNodeHealth attaches the checks Ping runs after startup.
// Fake runtime skips the vz probe: CI and --runtime=fake are not Apple Silicon.
func configureNodeHealth(np *node.StatusProvider, cfg dnconfig.Config) {
	if np == nil {
		return
	}
	np.CacheDir = cfg.CacheDir
	if cfg.Runtime != types.RuntimeFake {
		np.Runtime = func(context.Context) error { return vzrt.Available() }
	}
}

func newRuntime(cfg dnconfig.Config) (runtime.Runtime, error) {
	switch cfg.Runtime {
	case types.RuntimeFake:
		return fake.New(), nil
	default:
		return vzrt.New(runtime.Options{
			CacheDir:      cfg.CacheDir,
			NetworkMode:   cfg.NetworkMode,
			BridgeDevice:  cfg.BridgeInterface,
			Graphics:      cfg.Graphics,
			HostAgentVer:  dnconfig.Version,
			SerialConsole: cfg.SerialConsole,
		}), nil
	}
}

func runVK(ctx context.Context, cfg dnconfig.Config, k8s kubernetes.Interface, p *provider.Provider, serving dnconfig.CertificateProvider, skew *node.SkewCordon) error {
	if err := dnconfig.RequireKubeletClientCA(false, cfg.ClientCA); err != nil {
		return err
	}
	bundle, err := loadClientCA(cfg.ClientCA)
	if err != nil {
		return fmt.Errorf("kubelet client CA: %w", err)
	}
	if bundle == nil {
		// loadClientCA treats an empty path as NoAuth. Cluster startup must
		// not install that handler, even if the string check above was skipped.
		return fmt.Errorf("kubelet client CA: refusing to install anonymous kubelet auth")
	}
	go bundle.Run(ctx)
	// Server certificate only. The client CA, when set, is the live bundle
	// above so a rotated file is picked up without pinning ClientCAs here.
	tlsCfg, err := dnconfig.TLSConfigFromSource(cfg.CertPath, cfg.KeyPath, "", serving)
	if err != nil {
		return fmt.Errorf("kubelet tls: %w", err)
	}
	// Provider routes (exec, logs, stats) attach to this mux. Auth wraps it;
	// the mux itself stays the inner handler so routes registered later are served.
	mux := http.NewServeMux()
	n, err := nodeutil.NewNode(cfg.NodeName,
		func(pc nodeutil.ProviderConfig) (nodeutil.Provider, vknode.NodeProvider, error) {
			p.SetCredentialResolver(provider.KubeResolver{Client: k8s})
			np := node.NewStatusProvider(p.Inventory())
			configureNodeHealth(np, cfg)
			if skew != nil {
				np.SetSkewCordon(skew)
			}
			// Ping before the first status is published. A failure stays
			// inside the conditions; Ping itself returns nil so the node
			// controller keeps patching them and renewing the lease.
			p.InstallHealth(ctx, np)
			if err := p.ConfigureNode(ctx, pc.Node); err != nil {
				return nil, nil, err
			}
			if skew != nil {
				skew.Apply(pc.Node)
			}
			return p, np, nil
		},
		nodeutil.WithClient(k8s),
		func(nc *nodeutil.NodeConfig) error {
			nc.Handler = mux
			nc.HTTPListenAddr = cfg.HTTPListenAddr()
			nc.InformerResyncPeriod = cfg.Resync
			nc.NumWorkers = cfg.Workers
			if cfg.ProviderID != "" {
				nc.NodeSpec.Spec.ProviderID = cfg.ProviderID
			}
			return nil
		},
		// WebhookAuth with the live client CA. runVK refuses a nil bundle:
		// that path is nodeutil.NoAuth (anonymous after TLS).
		kubeletAuth(cfg.NodeName, bundle),
		nodeutil.WithTLSConfig(func(c *tls.Config) error {
			applyKubeletTLS(c, tlsCfg, bundle)
			return nil
		}),
		nodeutil.AttachProviderRoutes(mux),
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
