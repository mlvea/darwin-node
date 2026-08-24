package netutil

import (
	"crypto/rand"
	"fmt"
	"net"
	"strings"
)

// RandomMAC returns a locally-administered unicast MAC.
// Derived from Agoda macOS-vz-kubelet internal/netutil (Apache-2.0).
func RandomMAC() (net.HardwareAddr, error) {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("rand mac: %w", err)
	}
	buf[0] = (buf[0] | 0x02) & 0xfe
	return net.HardwareAddr(buf), nil
}

// NormalizeMAC lowercases and strips leading zeros per octet (ARP table style).
// Derived from Agoda macOS-vz-kubelet NormalizeMACAddress (Apache-2.0).
func NormalizeMAC(mac string) string {
	parts := strings.Split(mac, ":")
	for i, p := range parts {
		p = strings.ToLower(p)
		if len(p) == 2 && p[0] == '0' {
			p = p[1:]
		}
		parts[i] = p
	}
	return strings.Join(parts, ":")
}
