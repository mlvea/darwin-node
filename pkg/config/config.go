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

	ReservedCPU       resource.Quantity
	ReservedMemory    resource.Quantity
	ReservedEphemeral resource.Quantity

	Graphics types.Graphics

	CacheDir         string
	AllowedHostPaths []string // empty denies all hostPath
	KubeConfig       string
	CertPath         string
	KeyPath          string
	ClientCA         string
	ListenAddress    string // empty binds all interfaces
	ListenPort       int32
	LogLevel         string
	StartupTimeout   time.Duration
	Resync           time.Duration
	Workers          int

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

	OTELEndpoint string

	// AgentTCPFallback enables plaintext TCP to the guest agent (opt-in; vsock is primary).
	AgentTCPFallback bool
}

// Default returns production defaults.
func Default() Config {
	home, _ := os.UserHomeDir()
	cache := filepath.Join(home, "Library", "Caches", types.DefaultCacheBundleID)
	host, _ := os.Hostname()
	return Config{
		NodeName:          strings.ToLower(host),
		Runtime:           types.RuntimeVZ,
		MaxVMs:            types.AppleMaxConcurrentVMs,
		NetworkMode:       types.NetworkNAT,
		ReservedCPU:       resource.MustParse("2"),
		ReservedMemory:    resource.MustParse("4Gi"),
		ReservedEphemeral: resource.MustParse("20Gi"),
		Graphics:          types.DefaultGraphics(),
		CacheDir:          cache,
		KubeConfig:        filepath.Join(home, ".kube", "config"),
		ListenPort:        10250,
		LogLevel:          "info",
		Resync:            time.Minute,
		Workers:           10,
		AgentReadyTimeout: 2 * time.Minute,
		IPTimeout:         60 * time.Second,
		ProbeInterval:     10 * time.Second,
		InitTimeout:       2 * time.Minute,
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
	if v := os.Getenv("DARWIN_NODE_AGENT_TCP_FALLBACK"); v != "" {
		c.AgentTCPFallback = v == "true" || v == "1"
	}
	return c.Validate()
}

// HTTPListenAddr is host:port for the kubelet HTTP server.
// An empty ListenAddress binds all interfaces (":10250").
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
	return nil
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
