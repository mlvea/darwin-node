package config

import (
	"strings"
	"testing"

	"github.com/darwin-node/darwin-node/pkg/types"
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
	t.Setenv("DARWIN_NODE_LISTEN_ADDRESS", "127.0.0.1")
	t.Setenv("KUBELET_PORT", "12345")
	c := Default()
	if err := c.ApplyEnv(); err != nil {
		t.Fatal(err)
	}
	if c.ListenAddress != "127.0.0.1" {
		t.Fatalf("%q", c.ListenAddress)
	}
	if c.ListenPort != 12345 {
		t.Fatalf("%d", c.ListenPort)
	}
	if got := c.HTTPListenAddr(); got != "127.0.0.1:12345" {
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
