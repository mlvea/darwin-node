package sidecar

import (
	"net"
	"strings"

	"github.com/docker/docker/api/types"
)

// RoutableIP is an address a sidecar probe may dial.
// Loopback, unspecified, link-local, and multicast belong to the node
// (or to cloud metadata), not to a container on its bridge.
func RoutableIP(s string) (string, bool) {
	raw := strings.TrimSpace(s)
	raw = strings.Trim(raw, "[]")
	ip := net.ParseIP(raw)
	if ip == nil || ip.IsLoopback() || ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return "", false
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String(), true
	}
	return ip.String(), true
}

func containerIP(settings *types.NetworkSettings, prefer string) string {
	if settings == nil {
		return ""
	}
	if prefer != "" {
		if ep := settings.Networks[prefer]; ep != nil {
			if ip, ok := RoutableIP(ep.IPAddress); ok {
				return ip
			}
		}
	}
	for name, ep := range settings.Networks {
		if ep == nil || name == "host" {
			continue
		}
		if ip, ok := RoutableIP(ep.IPAddress); ok {
			return ip
		}
	}
	return ""
}
