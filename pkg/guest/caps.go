package guest

import (
	"fmt"
	"sort"
)

// Capability names a peer may advertise. They are additive. An older
// agent that omits the list has an empty set and keeps the RPCs it
// already spoke. Unknown names are ignored, never a handshake failure.
const (
	CapWorkload    = "workload"
	CapExit        = "exit"
	CapMetrics     = "metrics"
	CapStdinWindow = "stdin-window"
	// CapAdopt is the reseed barrier. An older agent omits it. The host
	// treats a missing name as "this guest cannot reseed".
	CapAdopt = "adopt"
)

// HostCapabilities is what this host can use. GuestCapabilities is what
// this agent implements. The session's set is the intersection.
func HostCapabilities() []string {
	return []string{CapWorkload, CapExit, CapMetrics, CapStdinWindow, CapAdopt}
}

// GuestCapabilities is the list this agent reports. It is not the
// intersection; the host computes that per session.
func GuestCapabilities() []string {
	return []string{CapWorkload, CapExit, CapMetrics, CapStdinWindow, CapAdopt}
}

// IntersectCapabilities returns the names present on both sides, sorted
// and de-duplicated. Either side empty (an older peer) yields nil.
func IntersectCapabilities(host, guest []string) []string {
	if len(host) == 0 || len(guest) == 0 {
		return nil
	}
	have := make(map[string]struct{}, len(host))
	for _, name := range host {
		if name != "" {
			have[name] = struct{}{}
		}
	}
	seen := make(map[string]struct{})
	var out []string
	for _, name := range guest {
		if name == "" {
			continue
		}
		if _, ok := have[name]; !ok {
			continue
		}
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func capabilitySet(names []string) map[string]struct{} {
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name != "" {
			out[name] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// selectProtocol picks the highest version both sides speak. A host that
// omits the range (0,0) is a peer from before negotiation and gets the
// current version. A range that does not overlap is refused.
func selectProtocol(min, max int) (int, error) {
	if min == 0 && max == 0 {
		return ProtocolVersion, nil
	}
	if min < 1 {
		min = 1
	}
	if max == 0 {
		max = min
	}
	if max < min {
		return 0, fmt.Errorf("protocol range %d..%d", min, max)
	}
	low, high := MinProtocol, MaxProtocol
	if min > low {
		low = min
	}
	if max < high {
		high = max
	}
	if low > high {
		return 0, fmt.Errorf("no common protocol (host %d..%d, guest %d..%d)", min, max, MinProtocol, MaxProtocol)
	}
	return high, nil
}
