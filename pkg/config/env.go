package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mlvea/darwin-node/pkg/types"

	"k8s.io/apimachinery/pkg/api/resource"
)

// ApplyEnv overlays DARWIN_NODE_* (and a few kubelet-compat) variables
// and validates. A caller that also parses flags must MergeEnv, register
// those flags with the merged values as their defaults, then Validate.
// Calling ApplyEnv after parsing puts the environment back on top of an
// explicit flag.
func (c *Config) ApplyEnv() error {
	if err := c.MergeEnv(); err != nil {
		return err
	}
	return c.Validate()
}

// MergeEnv overlays DARWIN_NODE_* (and a few kubelet-compat) variables.
// It does not validate. An empty variable leaves the current value.
// DARWIN_NODE_METRICS_ADDRESS is the exception: an empty value turns
// metrics off, because LookupEnv distinguishes "unset" from "set empty".
func (c *Config) MergeEnv() error {
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
	if v := os.Getenv("DARWIN_NODE_CERT_DIR"); v != "" {
		c.CertDir = v
	}
	if v := os.Getenv("DARWIN_NODE_BOOTSTRAP_KUBECONFIG"); v != "" {
		c.BootstrapKubeconfig = v
	}
	if v := os.Getenv("DARWIN_NODE_ROTATE_SERVER_CERTIFICATES"); v != "" {
		on, err := parseBoolFlag("DARWIN_NODE_ROTATE_SERVER_CERTIFICATES", v)
		if err != nil {
			return err
		}
		c.RotateServerCertificates = on
	}
	if v := os.Getenv("DARWIN_NODE_ROTATE_CLIENT_CERTIFICATES"); v != "" {
		on, err := parseBoolFlag("DARWIN_NODE_ROTATE_CLIENT_CERTIFICATES", v)
		if err != nil {
			return err
		}
		c.RotateClientCertificates = on
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
	if v := os.Getenv("DARWIN_NODE_CACHE_WRITERS"); v != "" {
		c.CacheWriters = splitComma(v)
	}
	if v := os.Getenv("DARWIN_NODE_RESERVED_CPU"); v != "" {
		q, err := parseNonNegativeQuantity("DARWIN_NODE_RESERVED_CPU", v)
		if err != nil {
			return err
		}
		c.ReservedCPU = q
	}
	if v := os.Getenv("DARWIN_NODE_RESERVED_MEMORY"); v != "" {
		q, err := parseNonNegativeQuantity("DARWIN_NODE_RESERVED_MEMORY", v)
		if err != nil {
			return err
		}
		c.ReservedMemory = q
	}
	if v := os.Getenv("DARWIN_NODE_RESERVED_EPHEMERAL"); v != "" {
		q, err := parseNonNegativeQuantity("DARWIN_NODE_RESERVED_EPHEMERAL", v)
		if err != nil {
			return err
		}
		c.ReservedEphemeral = q
	}
	return nil
}

// parseNonNegativeQuantity accepts a Kubernetes quantity (2, 500m, 4Gi).
func parseNonNegativeQuantity(name, v string) (resource.Quantity, error) {
	q, err := resource.ParseQuantity(strings.TrimSpace(v))
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("%s: %w", name, err)
	}
	if q.Sign() < 0 {
		return resource.Quantity{}, fmt.Errorf("%s: negative", name)
	}
	return q, nil
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

func parseBoolFlag(name, v string) (bool, error) {
	switch v {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("%s: want true or false, got %q", name, v)
	}
}

func first(a, b string) string {
	if a != "" {
		return a
	}
	return b
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
