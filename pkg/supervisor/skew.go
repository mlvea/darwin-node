package supervisor

import (
	"encoding/json"
	"fmt"
)

// WireMajor is the version-skew contract this node-agent speaks.
// Equal majors re-adopt. A different major keeps the VM running and
// occupies the slot. ProtocolMajor is a separate frame check.
const WireMajor = 1

// WireMinor is the additive field set of the handshake. Adding a
// field requires bumping this and the key list in
// TestHandshakeMinorTracksFieldSet together. Peers ignore unknown
// JSON, so a higher minor is still re-adopted.
const WireMinor = 0

// Optional capabilities. An advertised name this agent does not know
// does not turn a feature on.
const (
	CapHeartbeat = "heartbeat"
	CapPause     = "pause"
)

// Action is what the node-agent does with a supervisor it can reach.
type Action int

const (
	// ActionAdopt re-attaches the VM. build_sha and wire_minor do not
	// change this when wire_major matches.
	ActionAdopt Action = iota
	// ActionHold leaves the VM running, counts the slot, and waits for
	// the supervisor to exit on its own.
	ActionHold
)

func (a Action) String() string {
	switch a {
	case ActionAdopt:
		return "adopt"
	case ActionHold:
		return "hold"
	default:
		return "unknown"
	}
}

// Handshake is the version-skew report on the supervisor hello.
// Legacy is true only when wire_major was absent.
type Handshake struct {
	WireMajor       int
	WireMinor       int
	BuildSHA        string
	Capabilities    []string
	Slot            int
	PodUID          string
	HostBootID      string
	WorkloadStarted bool
	WorkloadExited  bool
	ExitCode        *int32
	Legacy          bool
}

func (h Handshake) clone() Handshake {
	out := h
	out.Capabilities = append([]string(nil), h.Capabilities...)
	if h.ExitCode != nil {
		code := *h.ExitCode
		out.ExitCode = &code
	}
	return out
}

// Allows reports whether feature may be used. The feature has to be
// one this agent knows and one the peer listed. Unknown names stay off.
func (h Handshake) Allows(feature string) bool {
	if !knownCapability(feature) {
		return false
	}
	for _, c := range h.Capabilities {
		if c == feature {
			return true
		}
	}
	return false
}

// WorkloadNote is the host's record that the guest workload ran.
type WorkloadNote struct {
	Started  bool
	Exited   bool
	ExitCode *int32
}

// SkewControl is how a held supervisor occupies a VM slot and cordons
// the node. The engine implements it. Release is idempotent.
type SkewControl interface {
	Hold(uid, reason string) error
	Release(uid string)
}

// Decide returns ActionAdopt when localMajor equals the peer's wire
// major. Anything else, including a supervisor with no version fields,
// is ActionHold. wire_minor and build_sha are not compared.
func Decide(localMajor int, hs Handshake) Action {
	if hs.Legacy || hs.WireMajor != localMajor {
		return ActionHold
	}
	return ActionAdopt
}

// MayStartWorkload is false once the handshake says the workload
// already ran. Calling it again would re-run a restartPolicy Never job.
func MayStartWorkload(hs Handshake) bool {
	return !hs.WorkloadStarted
}

// WorkloadResult is the guest exit code when the handshake says the
// workload has already finished.
func WorkloadResult(hs Handshake) (int32, bool) {
	if !hs.WorkloadExited || hs.ExitCode == nil {
		return 0, false
	}
	return *hs.ExitCode, true
}

// HoldReason is the cordon and event text. It names both wire majors.
func HoldReason(local int, hs Handshake) string {
	if hs.Legacy {
		return fmt.Sprintf("supervisor has no version handshake (wire major 0); this node-agent speaks wire major %d; the VM was left running", local)
	}
	return fmt.Sprintf("supervisor wire major %d; node-agent wire major %d; VM left running", hs.WireMajor, local)
}

// ParseHandshake reads a hello or a bare handshake object. Missing
// wire_major is wire major 0 and Legacy. Unknown fields are ignored.
func ParseHandshake(raw []byte) (Handshake, error) {
	var msg message
	if err := json.Unmarshal(raw, &msg); err != nil {
		return Handshake{}, err
	}
	return handshakeFromMessage(msg), nil
}

// MarshalHandshake emits the current field set, including zero values
// of wire_minor, slot, and the workload flags, so a new field cannot
// land without showing up in the key set.
func MarshalHandshake(hs Handshake) ([]byte, error) {
	major := hs.WireMajor
	wire := handshakeWire{
		WireMajor:       &major,
		WireMinor:       hs.WireMinor,
		BuildSHA:        hs.BuildSHA,
		Capabilities:    hs.Capabilities,
		Slot:            hs.Slot,
		PodUID:          hs.PodUID,
		HostBootID:      hs.HostBootID,
		WorkloadStarted: hs.WorkloadStarted,
		WorkloadExited:  hs.WorkloadExited,
		ExitCode:        hs.ExitCode,
	}
	return json.Marshal(wire)
}

// handshakeWire is the documented field set. Do not add omitempty:
// TestHandshakeMinorTracksFieldSet counts every key, including zeros.
type handshakeWire struct {
	WireMajor       *int     `json:"wire_major"`
	WireMinor       int      `json:"wire_minor"`
	BuildSHA        string   `json:"build_sha"`
	Capabilities    []string `json:"capabilities"`
	Slot            int      `json:"slot"`
	PodUID          string   `json:"pod_uid"`
	HostBootID      string   `json:"host_boot_id"`
	WorkloadStarted bool     `json:"workload_started"`
	WorkloadExited  bool     `json:"workload_exited"`
	ExitCode        *int32   `json:"exit_code"`
}

func handshakeFromMessage(msg message) Handshake {
	hs := Handshake{
		WireMinor:       msg.WireMinor,
		BuildSHA:        msg.BuildSHA,
		Capabilities:    append([]string(nil), msg.Capabilities...),
		Slot:            msg.Slot,
		PodUID:          msg.PodUID,
		HostBootID:      msg.HostBootID,
		WorkloadStarted: msg.WorkloadStarted,
		WorkloadExited:  msg.WorkloadExited,
	}
	if msg.WireMajor == nil {
		hs.Legacy = true
	} else {
		hs.WireMajor = *msg.WireMajor
	}
	if msg.ExitCode != nil {
		code := *msg.ExitCode
		hs.ExitCode = &code
	}
	return hs
}

func knownCapability(feature string) bool {
	switch feature {
	case CapHeartbeat, CapPause:
		return true
	default:
		return false
	}
}

func defaultCapabilities() []string {
	return []string{CapHeartbeat, CapPause}
}
