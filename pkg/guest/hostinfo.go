package guest

import (
	"net"
	"os"
	"syscall"
)

func defaultNetInfo() (NetInfoRes, error) {
	host, _ := os.Hostname()
	var ips []string
	var primary string
	ifaces, err := net.Interfaces()
	if err != nil {
		return NetInfoRes{}, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := iface.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.IsLoopback() {
				continue
			}
			if v4 := ipn.IP.To4(); v4 != nil {
				s := v4.String()
				ips = append(ips, s)
				if primary == "" {
					primary = s
				}
			}
		}
		if primary != "" {
			return NetInfoRes{
				PrimaryIP: primary,
				IPs:       ips,
				MAC:       iface.HardwareAddr.String(),
				IFName:    iface.Name,
			}, nil
		}
	}
	_ = host
	return NetInfoRes{PrimaryIP: primary, IPs: ips}, nil
}

func defaultMetrics() (MetricsRes, error) {
	// Best-effort without gopsutil so the guest package stays light.
	// CPU/memory stay zero rather than inventing usage; Statfs is real.
	var fsCap, fsUsed uint64
	var st syscall.Statfs_t
	if err := syscall.Statfs("/", &st); err == nil && st.Bsize > 0 {
		bsize := uint64(st.Bsize)
		fsCap = st.Blocks * bsize
		if st.Blocks > st.Bfree {
			fsUsed = (st.Blocks - st.Bfree) * bsize
		}
	}
	return MetricsRes{
		FsCapBytes:  fsCap,
		FsUsedBytes: fsUsed,
	}, nil
}
