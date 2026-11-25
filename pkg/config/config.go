package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/darwin-node/darwin-node/pkg/types"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Version is injected by ldflags.
var Version = "dev"

// Config is the node agent configuration.
type Config struct {
	NodeName          string
	ProviderID        string
	Runtime           types.RuntimeName
	MaxVMs            int
	NetworkMode       types.NetworkMode
	BridgeInterface   string
	AllowNATWorkloads bool
	DisableTaint      bool

	// WarmSlots keeps up to N pre-booted idle VMs in free slots so matching
	// pods adopt them instead of cold-booting. Warm VMs consume real slots.
	WarmSlots int
	// WarmImage is the image pre-booted into warm slots. Empty defers to the
	// most recently used image on this node.
	WarmImage string

	ReservedCPU       resource.Quantity
	ReservedMemory    resource.Quantity
	ReservedEphemeral resource.Quantity

	Graphics types.Graphics

	CacheDir         string
	AllowedHostPaths []string // empty denies all hostPath
	KubeConfig       string
	// Cluster is set when the process joins a cluster. Host directory
	// image paths are rejected. --standalone leaves it false.
	Cluster        bool
	CertPath       string
	KeyPath        string
	ClientCA       string
	ListenAddress  string // default 127.0.0.1; empty binds all interfaces
	ListenPort     int32
	LogLevel       string
	StartupTimeout time.Duration
	Resync         time.Duration
	Workers        int

	AgentReadyTimeout time.Duration
	IPTimeout         time.Duration
	ProbeInterval     time.Duration
	InitTimeout       time.Duration

	SSHUser           string
	SSHPrivateKey     string
	SSHPrivateKeyB64  string
	SSHPassword       string // deprecated: Validate fails if set (password auth removed)
	EnableSSHFallback bool
	SSHKnownHosts     string
	SSHHostKey        string // OpenSSH authorized_keys line; pins the guest host key

	InsecureRegistries []string

	// Image pull limits. Zero means the image package default, which is
	// also what Default sets so the flags show the real value.
	// MaxImageBytes caps a declared or decompressed disk. A 128 GiB sparse
	// macOS disk fits in 256 GiB; a 1 PiB annotation does not.
	MaxImageBytes           int64
	MaxImageCompressedBytes int64
	ImageDiskHeadroom       int64
	ImagePullTimeout        time.Duration
	ImageReadStall          time.Duration

	OTELEndpoint string

	// SerialConsole attaches a VM serial port for break-glass access via
	// `darwin-node console`. Opt-in; independent of the guest agent.
	SerialConsole bool

	// ShutdownGrace bounds the total drain budget on SIGTERM/SIGINT: pods
	// are deleted with their termination grace periods until it runs out.
	ShutdownGrace time.Duration

	// ImageGC deletes unused images and cache stores when the cache
	// filesystem is fuller than ImageGCHigh percent, until it is at or
	// below ImageGCLow. The defaults match kubelet (85 and 80).
	ImageGC         bool
	ImageGCHigh     int
	ImageGCLow      int
	ImageGCInterval time.Duration

	// MetricsAddress is the Prometheus listener. The default port is
	// 9102 so it does not take node_exporter's 9100. "off" disables it.
	MetricsAddress string
}

const (
	// DefaultMetricsAddress is the Prometheus bind. It is not 9100.
	DefaultMetricsAddress = "127.0.0.1:9102"
	// DefaultImageGCHigh is the kubelet image-gc-high-threshold.
	DefaultImageGCHigh = 85
	// DefaultImageGCLow is the kubelet image-gc-low-threshold.
	DefaultImageGCLow = 80
	// DefaultImageGCInterval is how often disk GC runs.
	DefaultImageGCInterval = 5 * time.Minute
)

// Default returns production defaults.
func Default() Config {
	home, _ := os.UserHomeDir()
	cache := filepath.Join(home, "Library", "Caches", types.DefaultCacheBundleID)
	host, _ := os.Hostname()
	return Config{
		NodeName:                strings.ToLower(host),
		Runtime:                 types.RuntimeVZ,
		MaxVMs:                  types.AppleMaxConcurrentVMs,
		NetworkMode:             types.NetworkNAT,
		ReservedCPU:             resource.MustParse("2"),
		ReservedMemory:          resource.MustParse("4Gi"),
		ReservedEphemeral:       resource.MustParse("20Gi"),
		Graphics:                types.DefaultGraphics(),
		CacheDir:                cache,
		KubeConfig:              filepath.Join(home, ".kube", "config"),
		ListenAddress:           "127.0.0.1",
		ListenPort:              10250,
		LogLevel:                "info",
		Resync:                  time.Minute,
		Workers:                 10,
		AgentReadyTimeout:       2 * time.Minute,
		IPTimeout:               60 * time.Second,
		ProbeInterval:           10 * time.Second,
		InitTimeout:             2 * time.Minute,
		ShutdownGrace:           60 * time.Second,
		ImageGC:                 true,
		ImageGCHigh:             DefaultImageGCHigh,
		ImageGCLow:              DefaultImageGCLow,
		ImageGCInterval:         DefaultImageGCInterval,
		MetricsAddress:          DefaultMetricsAddress,
		MaxImageBytes:           256 << 30,
		MaxImageCompressedBytes: 256 << 30,
		ImageDiskHeadroom:       10 << 30,
		ImagePullTimeout:        30 * time.Minute,
		ImageReadStall:          2 * time.Minute,
	}
}

// ApplyEnv overlays DARWIN_NODE_* (and a few kubelet-compat) variables.
func (c *Config) ApplyEnv() error {
	if v := os.Getenv("DARWIN_NODE_NAME"); v != "" {
		c.NodeName = v
	}
	if v := os.Getenv("DARWIN_NODE_RUNTIME"); v != "" {
		c.Runtime = types.RuntimeName(v)
	}
	if v := os.Getenv("DARWIN_NODE_MAX_VMS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("DARWIN_NODE_MAX_VMS: %w", err)
		}
		c.MaxVMs = n
	}
	if v := os.Getenv("DARWIN_NODE_NETWORK_MODE"); v != "" {
		c.NetworkMode = types.NetworkMode(v)
	}
	if v := os.Getenv("DARWIN_NODE_BRIDGE_INTERFACE"); v != "" {
		c.BridgeInterface = v
		if c.NetworkMode == types.NetworkNAT {
			c.NetworkMode = types.NetworkBridged
		}
	}
	if v := os.Getenv("VZ_BRIDGE_INTERFACE"); v != "" && c.BridgeInterface == "" {
		c.BridgeInterface = v
		c.NetworkMode = types.NetworkBridged
	}
	if v := os.Getenv("DARWIN_NODE_ALLOW_NAT_WORKLOADS"); v != "" {
		c.AllowNATWorkloads = v == "true" || v == "1"
	}
	if v := os.Getenv("DARWIN_NODE_CACHE_DIR"); v != "" {
		c.CacheDir = v
	}
	if v := os.Getenv("DARWIN_NODE_ALLOWED_HOST_PATHS"); v != "" {
		c.AllowedHostPaths = splitComma(v)
	}
	if v := os.Getenv("DARWIN_NODE_GRAPHICS"); v != "" {
		g, err := ParseGraphics(v)
		if err != nil {
			return err
		}
		c.Graphics = g
	}
	if v := os.Getenv("KUBECONFIG"); v != "" {
		c.KubeConfig = v
	}
	if v := os.Getenv("APISERVER_CERT_LOCATION"); v != "" {
		c.CertPath = v
	}
	if v := os.Getenv("APISERVER_KEY_LOCATION"); v != "" {
		c.KeyPath = v
	}
	if v := os.Getenv("APISERVER_CA_CERT_LOCATION"); v != "" {
		c.ClientCA = v
	}
	if v := os.Getenv("DARWIN_NODE_LISTEN_ADDRESS"); v != "" {
		c.ListenAddress = v
	}
	if v := os.Getenv("KUBELET_PORT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return err
		}
		c.ListenPort = int32(n)
	}
	if v := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); v != "" {
		c.OTELEndpoint = v
	}
	if v := os.Getenv("DARWIN_NODE_SSH_USER"); v != "" {
		c.SSHUser = v
	} else if v := os.Getenv("VZ_SSH_USER"); v != "" {
		c.SSHUser = v
	}
	c.SSHPrivateKey = first(os.Getenv("DARWIN_NODE_SSH_PRIVATE_KEY_PATH"), os.Getenv("VZ_SSH_PRIVATE_KEY_PATH"))
	c.SSHPrivateKeyB64 = first(os.Getenv("DARWIN_NODE_SSH_PRIVATE_KEY_BASE64"), os.Getenv("VZ_SSH_PRIVATE_KEY_BASE64"))
	c.SSHPassword = first(os.Getenv("DARWIN_NODE_SSH_PASSWORD"), os.Getenv("VZ_SSH_PASSWORD"))
	if v := os.Getenv("DARWIN_NODE_ENABLE_SSH_FALLBACK"); v != "" {
		c.EnableSSHFallback = v == "true" || v == "1"
	}
	if v := os.Getenv("DARWIN_NODE_SSH_KNOWN_HOSTS"); v != "" {
		c.SSHKnownHosts = v
	}
	if v := os.Getenv("DARWIN_NODE_SSH_HOST_KEY"); v != "" {
		c.SSHHostKey = v
	}
	if v := os.Getenv("DARWIN_NODE_INSECURE_REGISTRIES"); v != "" {
		c.InsecureRegistries = splitComma(v)
	}
	if v := os.Getenv("DARWIN_NODE_MAX_IMAGE_BYTES"); v != "" {
		n, err := parseByteSize("DARWIN_NODE_MAX_IMAGE_BYTES", v)
		if err != nil {
			return err
		}
		c.MaxImageBytes = n
	}
	if v := os.Getenv("DARWIN_NODE_MAX_IMAGE_COMPRESSED_BYTES"); v != "" {
		n, err := parseByteSize("DARWIN_NODE_MAX_IMAGE_COMPRESSED_BYTES", v)
		if err != nil {
			return err
		}
		c.MaxImageCompressedBytes = n
	}
	if v := os.Getenv("DARWIN_NODE_IMAGE_DISK_HEADROOM"); v != "" {
		n, err := parseByteSize("DARWIN_NODE_IMAGE_DISK_HEADROOM", v)
		if err != nil {
			return err
		}
		c.ImageDiskHeadroom = n
	}
	if v := os.Getenv("DARWIN_NODE_IMAGE_PULL_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("DARWIN_NODE_IMAGE_PULL_TIMEOUT: %w", err)
		}
		c.ImagePullTimeout = d
	}
	if v := os.Getenv("DARWIN_NODE_IMAGE_READ_STALL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("DARWIN_NODE_IMAGE_READ_STALL: %w", err)
		}
		c.ImageReadStall = d
	}
	if v := os.Getenv("DARWIN_NODE_AGENT_READY_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("DARWIN_NODE_AGENT_READY_TIMEOUT: %w", err)
		}
		c.AgentReadyTimeout = d
	}
	// DARWIN_NODE_AGENT_TCP_FALLBACK is ignored. The guest agent is
	// reached only over vsock. The variable used to turn on a plaintext
	// TCP dial that sent the pod token in the clear.
	if v := os.Getenv("DARWIN_NODE_WARM_SLOTS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("DARWIN_NODE_WARM_SLOTS: %w", err)
		}
		c.WarmSlots = n
	}
	if v := os.Getenv("DARWIN_NODE_WARM_IMAGE"); v != "" {
		c.WarmImage = v
	}
	if v := os.Getenv("DARWIN_NODE_SERIAL_CONSOLE"); v != "" {
		c.SerialConsole = v == "true" || v == "1"
	}
	if v := os.Getenv("DARWIN_NODE_SHUTDOWN_GRACE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("DARWIN_NODE_SHUTDOWN_GRACE: %w", err)
		}
		c.ShutdownGrace = d
	}
	if v := os.Getenv("DARWIN_NODE_IMAGE_GC"); v != "" {
		switch v {
		case "true", "1":
			c.ImageGC = true
		case "false", "0":
			c.ImageGC = false
		default:
			return fmt.Errorf("DARWIN_NODE_IMAGE_GC: want true or false, got %q", v)
		}
	}
	if v := os.Getenv("DARWIN_NODE_IMAGE_GC_HIGH_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("DARWIN_NODE_IMAGE_GC_HIGH_THRESHOLD: %w", err)
		}
		c.ImageGCHigh = n
	}
	if v := os.Getenv("DARWIN_NODE_IMAGE_GC_LOW_THRESHOLD"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("DARWIN_NODE_IMAGE_GC_LOW_THRESHOLD: %w", err)
		}
		c.ImageGCLow = n
	}
	if v := os.Getenv("DARWIN_NODE_IMAGE_GC_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("DARWIN_NODE_IMAGE_GC_INTERVAL: %w", err)
		}
		c.ImageGCInterval = d
	}
	if v, ok := os.LookupEnv("DARWIN_NODE_METRICS_ADDRESS"); ok {
		c.MetricsAddress = v
	}
	return c.Validate()
}

// MetricsDisabled reports that addr turns the Prometheus listener off.
// An empty address, "off", "disabled", and "-" are off. The default
// address is not empty.
func MetricsDisabled(addr string) bool {
	switch strings.ToLower(strings.TrimSpace(addr)) {
	case "", "off", "disabled", "-":
		return true
	default:
		return false
	}
}

// HTTPListenAddr is host:port for the kubelet HTTP server.
// The default ListenAddress is 127.0.0.1. An empty ListenAddress binds
// all interfaces (":10250"); set that only when every interface is trusted.
func (c Config) HTTPListenAddr() string {
	return net.JoinHostPort(c.ListenAddress, strconv.Itoa(int(c.ListenPort)))
}

func splitComma(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Validate checks invariants.
func (c Config) Validate() error {
	if c.MaxVMs < 1 || c.MaxVMs > types.AppleMaxConcurrentVMs {
		return fmt.Errorf("max VMs must be 1 or 2 (Apple EULA), got %d", c.MaxVMs)
	}
	if c.WarmSlots < 0 || c.WarmSlots > types.AppleMaxConcurrentVMs {
		return fmt.Errorf("warm slots must be between 0 and %d (Apple EULA), got %d", types.AppleMaxConcurrentVMs, c.WarmSlots)
	}
	if c.ShutdownGrace < 0 {
		return fmt.Errorf("shutdown grace must not be negative, got %s", c.ShutdownGrace)
	}
	switch c.Runtime {
	case types.RuntimeVZ, types.RuntimeFake, "":
	default:
		return fmt.Errorf("unknown runtime %q", c.Runtime)
	}
	switch c.NetworkMode {
	case types.NetworkNAT, types.NetworkBridged, types.NetworkDisabled:
	default:
		return fmt.Errorf("unknown network mode %q", c.NetworkMode)
	}
	if c.NetworkMode == types.NetworkBridged && c.BridgeInterface == "" {
		return fmt.Errorf("bridged mode requires a bridge interface")
	}
	if c.SSHPassword != "" {
		return fmt.Errorf("password auth removed: DARWIN_NODE_SSH_PASSWORD and VZ_SSH_PASSWORD are no longer supported")
	}
	if c.MaxImageBytes < 0 || c.MaxImageCompressedBytes < 0 || c.ImageDiskHeadroom < 0 {
		return fmt.Errorf("image size limits must not be negative")
	}
	if c.ImagePullTimeout < 0 || c.ImageReadStall < 0 {
		return fmt.Errorf("image pull timeouts must not be negative")
	}
	if c.ImageGC {
		if c.ImageGCHigh < 1 || c.ImageGCHigh > 100 {
			return fmt.Errorf("image GC high threshold must be 1-100, got %d", c.ImageGCHigh)
		}
		if c.ImageGCLow < 0 || c.ImageGCLow >= c.ImageGCHigh {
			return fmt.Errorf("image GC low threshold must be below the high threshold %d, got %d", c.ImageGCHigh, c.ImageGCLow)
		}
	}
	if c.ImageGCInterval < 0 {
		return fmt.Errorf("image GC interval must not be negative, got %s", c.ImageGCInterval)
	}
	if !MetricsDisabled(c.MetricsAddress) {
		if _, _, err := net.SplitHostPort(c.MetricsAddress); err != nil {
			return fmt.Errorf("metrics address: %w", err)
		}
	}
	if c.AgentReadyTimeout < 0 {
		return fmt.Errorf("agent ready timeout must not be negative")
	}
	return nil
}

// parseByteSize accepts a Kubernetes quantity (256Gi, 10Gi) or a plain integer.
func parseByteSize(name, v string) (int64, error) {
	q, err := resource.ParseQuantity(v)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", name, err)
	}
	n := q.Value()
	if n < 0 {
		return 0, fmt.Errorf("%s: negative", name)
	}
	return n, nil
}

// ParseGraphics parses "1920x1200@80" or "none".
func ParseGraphics(s string) (types.Graphics, error) {
	s = strings.TrimSpace(s)
	if s == "" || s == "default" {
		return types.DefaultGraphics(), nil
	}
	if s == "none" {
		return types.Graphics{Enabled: false}, nil
	}
	var w, h, ppi int
	n, err := fmt.Sscanf(s, "%dx%d@%d", &w, &h, &ppi)
	if err != nil || n != 3 || w <= 0 || h <= 0 || ppi <= 0 {
		return types.Graphics{}, fmt.Errorf("graphics must be WxH@PPI or none, got %q", s)
	}
	return types.Graphics{Enabled: true, Width: w, Height: h, PPI: ppi}, nil
}
