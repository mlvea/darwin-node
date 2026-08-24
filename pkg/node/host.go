package node

import (
	"context"
	"fmt"
	"os"
	"runtime"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
	psnet "github.com/shirou/gopsutil/v4/net"
)

// ProbeHost reads the current machine. Safe to call from tests; may return
// zeros on failure (caller should treat that as error).
func ProbeHost(ctx context.Context) (Host, error) {
	h := Host{Arch: runtime.GOARCH}
	if n, err := os.Hostname(); err == nil {
		h.Hostname = n
	}
	if v, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		h.MemoryBytes = int64(v.Total)
		h.MemoryUsedFrac = v.UsedPercent / 100
	}
	if c, err := cpu.CountsWithContext(ctx, true); err == nil {
		h.LogicalCPUs = int64(c)
	}
	if d, err := disk.UsageWithContext(ctx, "/"); err == nil {
		h.DiskBytes = int64(d.Total)
		if d.Total > 0 {
			h.DiskUsedFrac = float64(d.Used) / float64(d.Total)
		}
	}
	if info, err := host.InfoWithContext(ctx); err == nil {
		h.HostID = info.HostID
		h.Kernel = info.KernelVersion
		h.OSImage = info.OS + " " + info.PlatformVersion
		if info.KernelArch != "" {
			h.Arch = info.KernelArch
		}
	}
	if cs, err := cpu.InfoWithContext(ctx); err == nil && len(cs) > 0 {
		h.CPUModel = cs[0].ModelName
	}
	if ifs, err := psnet.InterfacesWithContext(ctx); err == nil {
		for _, iface := range ifs {
			if len(iface.Addrs) == 0 {
				continue
			}
			for _, a := range iface.Addrs {
				if a.Addr != "" {
					h.InternalIP = trimCIDR(a.Addr)
					break
				}
			}
			if h.InternalIP != "" {
				break
			}
		}
	}
	if h.LogicalCPUs == 0 || h.MemoryBytes == 0 {
		return h, fmt.Errorf("incomplete host probe (cpu=%d mem=%d)", h.LogicalCPUs, h.MemoryBytes)
	}
	return h, nil
}

func trimCIDR(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return s[:i]
		}
	}
	return s
}
