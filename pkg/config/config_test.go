package config

import (
	"strings"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/image"
	"github.com/mlvea/darwin-node/pkg/types"

	"k8s.io/apimachinery/pkg/api/resource"
)

func TestParseGraphics(t *testing.T) {
	g, err := ParseGraphics("2560x1600@110")
	if err != nil {
		t.Fatal(err)
	}
	if !g.Enabled || g.Width != 2560 || g.Height != 1600 || g.PPI != 110 {
		t.Fatalf("%+v", g)
	}
	g, err = ParseGraphics("none")
	if err != nil || g.Enabled {
		t.Fatalf("%+v %v", g, err)
	}
}

func TestValidateMaxVMs(t *testing.T) {
	c := Default()
	c.MaxVMs = 3
	if err := c.Validate(); err == nil {
		t.Fatal("expected error")
	}
	c.MaxVMs = 2
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultCap(t *testing.T) {
	c := Default()
	if c.MaxVMs != types.AppleMaxConcurrentVMs {
		t.Fatalf("max=%d", c.MaxVMs)
	}
}

func TestAllowedHostPathsEnv(t *testing.T) {
	c := Default()
	if len(c.AllowedHostPaths) != 0 {
		t.Fatalf("default allowlist must be empty, got %v", c.AllowedHostPaths)
	}
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "")
	t.Setenv("VZ_SSH_PASSWORD", "")
	t.Setenv("DARWIN_NODE_ALLOWED_HOST_PATHS", "/Volumes/Work, /var/tmp")
	if err := c.ApplyEnv(); err != nil {
		t.Fatal(err)
	}
	if len(c.AllowedHostPaths) != 2 || c.AllowedHostPaths[0] != "/Volumes/Work" || c.AllowedHostPaths[1] != "/var/tmp" {
		t.Fatalf("%v", c.AllowedHostPaths)
	}
}

func TestSSHKeyB64Env(t *testing.T) {
	t.Setenv("DARWIN_NODE_SSH_PRIVATE_KEY_BASE64", "YWJj")
	t.Setenv("DARWIN_NODE_INSECURE_REGISTRIES", "localhost:5000, registry.local")
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "")
	t.Setenv("VZ_SSH_PASSWORD", "")
	c := Default()
	if err := c.ApplyEnv(); err != nil {
		t.Fatal(err)
	}
	if c.SSHPrivateKeyB64 != "YWJj" {
		t.Fatalf("b64 %q", c.SSHPrivateKeyB64)
	}
	if len(c.InsecureRegistries) != 2 {
		t.Fatalf("%v", c.InsecureRegistries)
	}
	if c.EnableSSHFallback {
		t.Fatal("SSH fallback must default off")
	}
}

func TestPasswordAuthRemovedEnv(t *testing.T) {
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "")
	t.Setenv("VZ_SSH_PASSWORD", "master")
	c := Default()
	err := c.ApplyEnv()
	if err == nil || !strings.Contains(err.Error(), "password auth removed") {
		t.Fatalf("got %v", err)
	}
}

func TestPasswordAuthRemovedDARWINEnv(t *testing.T) {
	t.Setenv("VZ_SSH_PASSWORD", "")
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "secret")
	c := Default()
	err := c.ApplyEnv()
	if err == nil || !strings.Contains(err.Error(), "password auth removed") {
		t.Fatalf("got %v", err)
	}
}

func TestValidatePasswordAuthRemoved(t *testing.T) {
	c := Default()
	c.SSHPassword = "x"
	err := c.Validate()
	if err == nil || !strings.Contains(err.Error(), "password auth removed") {
		t.Fatalf("got %v", err)
	}
}

func TestRequireKubeletClientCA(t *testing.T) {
	if err := RequireKubeletClientCA(true, ""); err != nil {
		t.Fatal(err)
	}
	err := RequireKubeletClientCA(false, "")
	if err == nil || !strings.Contains(err.Error(), "client CA") {
		t.Fatalf("cluster mode with no client CA: %v", err)
	}
	err = RequireKubeletClientCA(false, " \t\n")
	if err == nil || !strings.Contains(err.Error(), "client CA") {
		t.Fatalf("whitespace client CA: %v", err)
	}
	if err := RequireKubeletClientCA(false, "/var/lib/darwin-node/ca.crt"); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultListenAddressIsLoopback(t *testing.T) {
	c := Default()
	if c.ListenAddress != "127.0.0.1" {
		t.Fatalf("default listen address %q, want 127.0.0.1", c.ListenAddress)
	}
	if got := c.HTTPListenAddr(); got != "127.0.0.1:10250" {
		t.Fatalf("default listen %s", got)
	}
}

func TestRequireKubeletTLS(t *testing.T) {
	if err := RequireKubeletTLS(true, Config{}); err != nil {
		t.Fatal(err)
	}
	if err := RequireKubeletTLS(false, Config{}); err == nil {
		t.Fatal("cluster mode empty certs")
	}
	if err := RequireKubeletTLS(false, Config{CertPath: "c"}); err == nil {
		t.Fatal("missing key")
	}
	if err := RequireKubeletTLS(false, Config{KeyPath: "k"}); err == nil {
		t.Fatal("missing cert")
	}
	if err := RequireKubeletTLS(false, Config{CertPath: " ", KeyPath: " "}); err == nil {
		t.Fatal("whitespace certs")
	}
	if err := RequireKubeletTLS(false, Config{CertPath: "c", KeyPath: "k"}); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPListenAddr(t *testing.T) {
	c := Config{ListenPort: 10250}
	if got := c.HTTPListenAddr(); got != ":10250" {
		t.Fatalf("all-interfaces %s", got)
	}
	c.ListenAddress = "127.0.0.1"
	if got := c.HTTPListenAddr(); got != "127.0.0.1:10250" {
		t.Fatalf("%s", got)
	}
	c.ListenAddress = "::1"
	if got := c.HTTPListenAddr(); got != "[::1]:10250" {
		t.Fatalf("%s", got)
	}
}

func TestListenAddressEnv(t *testing.T) {
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "")
	t.Setenv("VZ_SSH_PASSWORD", "")
	t.Setenv("DARWIN_NODE_LISTEN_ADDRESS", "10.1.2.3")
	t.Setenv("KUBELET_PORT", "12345")
	c := Default()
	if c.ListenAddress != "127.0.0.1" {
		t.Fatalf("default before env %q", c.ListenAddress)
	}
	if err := c.ApplyEnv(); err != nil {
		t.Fatal(err)
	}
	if c.ListenAddress != "10.1.2.3" {
		t.Fatalf("%q", c.ListenAddress)
	}
	if c.ListenPort != 12345 {
		t.Fatalf("%d", c.ListenPort)
	}
	if got := c.HTTPListenAddr(); got != "10.1.2.3:12345" {
		t.Fatalf("%s", got)
	}
}

func TestEnableSSHFallbackEnv(t *testing.T) {
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "")
	t.Setenv("VZ_SSH_PASSWORD", "")
	t.Setenv("DARWIN_NODE_ENABLE_SSH_FALLBACK", "true")
	t.Setenv("DARWIN_NODE_SSH_HOST_KEY", "ssh-ed25519 AAAA")
	c := Default()
	if c.EnableSSHFallback {
		t.Fatal("Default must leave SSH fallback off")
	}
	if err := c.ApplyEnv(); err != nil {
		t.Fatal(err)
	}
	if !c.EnableSSHFallback {
		t.Fatal("expected opt-in")
	}
	if c.SSHHostKey != "ssh-ed25519 AAAA" {
		t.Fatalf("host key %q", c.SSHHostKey)
	}
}

func TestAgentReadyTimeoutEnv(t *testing.T) {
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "")
	t.Setenv("VZ_SSH_PASSWORD", "")
	t.Setenv("DARWIN_NODE_AGENT_READY_TIMEOUT", "90s")
	c := Default()
	if c.AgentReadyTimeout != 2*time.Minute {
		t.Fatalf("default %s", c.AgentReadyTimeout)
	}
	if err := c.ApplyEnv(); err != nil {
		t.Fatal(err)
	}
	if c.AgentReadyTimeout != 90*time.Second {
		t.Fatalf("%s", c.AgentReadyTimeout)
	}
	c.AgentReadyTimeout = -time.Second
	if err := c.Validate(); err == nil {
		t.Fatal("negative agent ready timeout accepted")
	}
}

func TestDefaultImagePullLimits(t *testing.T) {
	c := Default()
	if c.MaxImageBytes != image.DefaultMaxImageBytes || c.MaxImageCompressedBytes != image.DefaultMaxCompressedBytes {
		t.Fatalf("byte caps %d %d", c.MaxImageBytes, c.MaxImageCompressedBytes)
	}
	if c.ImageDiskHeadroom != image.DefaultDiskHeadroom {
		t.Fatalf("headroom %d", c.ImageDiskHeadroom)
	}
	if c.ImagePullTimeout != image.DefaultPullTimeout || c.ImageReadStall != image.DefaultReadStall {
		t.Fatalf("timeouts %s %s", c.ImagePullTimeout, c.ImageReadStall)
	}
}

func TestImagePullLimitEnv(t *testing.T) {
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "")
	t.Setenv("VZ_SSH_PASSWORD", "")
	t.Setenv("DARWIN_NODE_MAX_IMAGE_BYTES", "32Mi")
	t.Setenv("DARWIN_NODE_MAX_IMAGE_COMPRESSED_BYTES", "8Mi")
	t.Setenv("DARWIN_NODE_IMAGE_DISK_HEADROOM", "1Gi")
	t.Setenv("DARWIN_NODE_IMAGE_PULL_TIMEOUT", "45s")
	t.Setenv("DARWIN_NODE_IMAGE_READ_STALL", "15s")
	c := Default()
	if err := c.ApplyEnv(); err != nil {
		t.Fatal(err)
	}
	if c.MaxImageBytes != 32<<20 || c.MaxImageCompressedBytes != 8<<20 || c.ImageDiskHeadroom != 1<<30 {
		t.Fatalf("bytes %d %d %d", c.MaxImageBytes, c.MaxImageCompressedBytes, c.ImageDiskHeadroom)
	}
	if c.ImagePullTimeout != 45*time.Second || c.ImageReadStall != 15*time.Second {
		t.Fatalf("durations %s %s", c.ImagePullTimeout, c.ImageReadStall)
	}
}

func TestImageGCAndMetricsDefaults(t *testing.T) {
	c := Default()
	if !c.ImageGC || c.ImageGCHigh != DefaultImageGCHigh || c.ImageGCLow != DefaultImageGCLow || c.ImageGCInterval != DefaultImageGCInterval {
		t.Fatalf("gc %+v", c)
	}
	if c.MetricsAddress != DefaultMetricsAddress || c.MetricsAddress == "127.0.0.1:9100" {
		t.Fatalf("metrics address %q", c.MetricsAddress)
	}
	c.ImageGCLow = c.ImageGCHigh
	if err := c.Validate(); err == nil {
		t.Fatal("low threshold equal to high was accepted")
	}
	c = Default()
	c.MetricsAddress = "not-a-port"
	if err := c.Validate(); err == nil {
		t.Fatal("invalid metrics address accepted")
	}
	c.MetricsAddress = "off"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !MetricsDisabled("") || !MetricsDisabled("disabled") || !MetricsDisabled("-") || MetricsDisabled(DefaultMetricsAddress) {
		t.Fatal("metrics disable set")
	}
}

func TestImageGCAndMetricsEnv(t *testing.T) {
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "")
	t.Setenv("VZ_SSH_PASSWORD", "")
	t.Setenv("DARWIN_NODE_IMAGE_GC", "false")
	t.Setenv("DARWIN_NODE_IMAGE_GC_HIGH_THRESHOLD", "90")
	t.Setenv("DARWIN_NODE_IMAGE_GC_LOW_THRESHOLD", "70")
	t.Setenv("DARWIN_NODE_IMAGE_GC_INTERVAL", "1m")
	t.Setenv("DARWIN_NODE_METRICS_ADDRESS", "127.0.0.1:19102")
	c := Default()
	if err := c.ApplyEnv(); err != nil {
		t.Fatal(err)
	}
	if c.ImageGC || c.ImageGCHigh != 90 || c.ImageGCLow != 70 || c.ImageGCInterval != time.Minute {
		t.Fatalf("gc %+v", c)
	}
	if c.MetricsAddress != "127.0.0.1:19102" {
		t.Fatalf("metrics %q", c.MetricsAddress)
	}
	t.Setenv("DARWIN_NODE_IMAGE_GC", "maybe")
	c = Default()
	if err := c.ApplyEnv(); err == nil {
		t.Fatal("bad DARWIN_NODE_IMAGE_GC accepted")
	}
}

func TestReservedEnv(t *testing.T) {
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "")
	t.Setenv("VZ_SSH_PASSWORD", "")
	t.Setenv("DARWIN_NODE_RESERVED_CPU", "1500m")
	t.Setenv("DARWIN_NODE_RESERVED_MEMORY", "6Gi")
	t.Setenv("DARWIN_NODE_RESERVED_EPHEMERAL", "8Gi")
	c := Default()
	if err := c.ApplyEnv(); err != nil {
		t.Fatal(err)
	}
	if c.ReservedCPU.Cmp(resource.MustParse("1500m")) != 0 {
		t.Fatalf("cpu %s", c.ReservedCPU.String())
	}
	if c.ReservedMemory.Cmp(resource.MustParse("6Gi")) != 0 {
		t.Fatalf("memory %s", c.ReservedMemory.String())
	}
	if c.ReservedEphemeral.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Fatalf("ephemeral %s", c.ReservedEphemeral.String())
	}

	t.Setenv("DARWIN_NODE_RESERVED_CPU", "-1")
	c = Default()
	if err := c.ApplyEnv(); err == nil {
		t.Fatal("negative DARWIN_NODE_RESERVED_CPU accepted")
	}
	t.Setenv("DARWIN_NODE_RESERVED_CPU", "nope")
	c = Default()
	if err := c.ApplyEnv(); err == nil || !strings.Contains(err.Error(), "DARWIN_NODE_RESERVED_CPU") {
		t.Fatalf("bad cpu quantity: %v", err)
	}
	c = Default()
	c.ReservedMemory = resource.MustParse("-1")
	if err := c.Validate(); err == nil {
		t.Fatal("negative reserved memory accepted")
	}
}

func TestImagePullLimitsRejectNegative(t *testing.T) {
	c := Default()
	c.MaxImageBytes = -1
	if err := c.Validate(); err == nil {
		t.Fatal("negative max image bytes accepted")
	}
	c = Default()
	c.ImagePullTimeout = -time.Second
	if err := c.Validate(); err == nil {
		t.Fatal("negative pull timeout accepted")
	}
	c = Default()
	c.ImageReadStall = -time.Second
	if err := c.Validate(); err == nil {
		t.Fatal("negative read stall accepted")
	}
}
