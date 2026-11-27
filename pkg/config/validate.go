package config

import (
	"fmt"
	"net"
	"strings"

	"github.com/mlvea/darwin-node/pkg/types"
)

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
	if c.ReservedCPU.Sign() < 0 || c.ReservedMemory.Sign() < 0 || c.ReservedEphemeral.Sign() < 0 {
		return fmt.Errorf("reserved resources must not be negative")
	}
	for _, w := range c.CacheWriters {
		if err := validateCacheWriter(w); err != nil {
			return err
		}
	}
	if c.RotateClientCertificates && strings.TrimSpace(c.BootstrapKubeconfig) == "" {
		return fmt.Errorf("client certificate rotation requires --bootstrap-kubeconfig")
	}
	if (c.RotateServerCertificates || c.RotateClientCertificates) && strings.TrimSpace(c.CertDir) == "" {
		return fmt.Errorf("certificate rotation requires --cert-dir")
	}
	return nil
}

// validateCacheWriter accepts "namespace" or "namespace/serviceaccount".
func validateCacheWriter(entry string) error {
	entry = strings.TrimSpace(entry)
	if entry == "" {
		return fmt.Errorf("cache writer entry is empty")
	}
	ns, sa, ok := strings.Cut(entry, "/")
	if !ok {
		return validateWriterSegment(entry)
	}
	if ns == "" || sa == "" || strings.Contains(sa, "/") {
		return fmt.Errorf("cache writer %q must be namespace or namespace/serviceaccount", entry)
	}
	if err := validateWriterSegment(ns); err != nil {
		return fmt.Errorf("cache writer %q: %w", entry, err)
	}
	if err := validateWriterSegment(sa); err != nil {
		return fmt.Errorf("cache writer %q: %w", entry, err)
	}
	return nil
}

func validateWriterSegment(s string) error {
	if s == "." || s == ".." {
		return fmt.Errorf("cache writer segment %q is not allowed", s)
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
		default:
			return fmt.Errorf("cache writer segment %q must match [A-Za-z0-9._-]+", s)
		}
	}
	return nil
}
