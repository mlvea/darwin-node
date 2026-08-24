package node

// UsageNanoCores converts a 0–100 CPU percent over logicalCPUs into kubelet
// nano-cores. Capacity (logical CPU count) must never be reported as usage.
func UsageNanoCores(logicalCPUs int64, percent float64) uint64 {
	if logicalCPUs <= 0 || percent <= 0 {
		return 0
	}
	if percent > 100 {
		percent = 100
	}
	return uint64(percent / 100.0 * float64(logicalCPUs) * 1e9)
}

// UsageBytes is used memory, not capacity.
func UsageBytes(total int64, usedFrac float64) uint64 {
	if total <= 0 || usedFrac <= 0 {
		return 0
	}
	if usedFrac > 1 {
		usedFrac = 1
	}
	return uint64(float64(total) * usedFrac)
}
