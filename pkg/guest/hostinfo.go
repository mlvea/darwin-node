package guest

import (
	"net"
	"os"
	"runtime"
	"sync"
	"syscall"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/mem"
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

// cpuNanoCores is the instantaneous rate between two aggregate CPU
// samples, in nano-cores (1e9 is one core busy for the whole interval).
// busy/total are cumulative CPU-seconds across every core. nCPU is the
// logical core count. A zero baseline, a non-positive delta, or a clock
// that went backwards is 0.
func cpuNanoCores(prevBusy, prevTotal, busy, total float64, nCPU int) uint64 {
	if nCPU <= 0 || (prevBusy == 0 && prevTotal == 0) {
		return 0
	}
	dt := total - prevTotal
	db := busy - prevBusy
	if dt <= 0 || db <= 0 {
		return 0
	}
	if db > dt {
		db = dt
	}
	return uint64(db / dt * float64(nCPU) * 1e9)
}

// metricsSample is the previous aggregate CPU reading. The first call
// has no baseline, so CPU is 0 until the next sample.
var (
	metricsMu   sync.Mutex
	metricsPrev cpuSample
	metricsHave bool
)

type cpuSample struct {
	busy, total float64
}

// defaultMetrics samples this guest. The numbers are telemetry for
// kubectl top. The host does not use them for scheduling. A guest is
// root on its own VM and can report anything; a missing or failing
// sample returns an error so the host can fall back to the VM's
// configured size instead of publishing zeros.
func defaultMetrics() (MetricsRes, error) {
	vm, err := mem.VirtualMemory()
	if err != nil {
		return MetricsRes{}, err
	}
	ws := uint64(0)
	if vm.Total >= vm.Available {
		ws = vm.Total - vm.Available
	}
	rss := vm.Used
	if rss == 0 {
		rss = ws
	}
	if ws == 0 {
		ws = rss
	}
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
		CPUNanoCores:     sampleCPU(),
		MemoryWorkingSet: ws,
		MemoryRSS:        rss,
		FsCapBytes:       fsCap,
		FsUsedBytes:      fsUsed,
	}, nil
}

func sampleCPU() uint64 {
	times, err := cpu.Times(false)
	if err != nil || len(times) == 0 {
		return 0
	}
	busy, total := cpuBusyTotal(times[0])
	metricsMu.Lock()
	defer metricsMu.Unlock()
	var nano uint64
	if metricsHave {
		nano = cpuNanoCores(metricsPrev.busy, metricsPrev.total, busy, total, runtime.NumCPU())
	}
	metricsPrev = cpuSample{busy: busy, total: total}
	metricsHave = true
	return nano
}

// cpuBusyTotal splits one aggregate TimesStat. Guest and GuestNice are
// already inside User and Nice on Linux, so they are not added again.
func cpuBusyTotal(t cpu.TimesStat) (busy, total float64) {
	idle := t.Idle + t.Iowait
	total = t.User + t.Nice + t.System + idle + t.Irq + t.Softirq + t.Steal
	if total < idle {
		return 0, total
	}
	return total - idle, total
}
