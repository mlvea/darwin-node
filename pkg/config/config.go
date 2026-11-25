package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mlvea/darwin-node/pkg/types"

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

	// CacheWriters is who may write a named cache back into the node
	// store. Each entry is a namespace, or namespace/serviceaccount.
	// Empty means no pod may snapshot a cache. A pod must also opt in
	// with cache-write.darwin.node/<name>: "true".
	CacheWriters []string
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
