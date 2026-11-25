package vzcontract

import (
	"encoding/json"
	"fmt"
)

// Order names select the Stop completion versus KVO permutation.
const (
	OrderKVOBeforeCompletion = "kvo_before_stop_completion"
	OrderCompletionBeforeKVO = "stop_completion_before_kvo"
	OrderJitter              = "jitter"
)

// Barrier names match the simulator's actor hooks.
const (
	BarrierBeforeGuestDidStop      = "before_guest_did_stop"
	BarrierBeforeKVOStopped        = "before_kvo_stopped"
	BarrierBeforeStopCompletion    = "before_stop_completion"
	BarrierBeforeKVOStarting       = "before_kvo_starting"
	BarrierBeforeKVORunning        = "before_kvo_running"
	BarrierBeforeStartCompletion   = "before_start_completion"
	BarrierBeforeKVOError          = "before_kvo_error"
	BarrierBeforeConnectCompletion = "before_connect_completion"
	BarrierBeforeSaveCompletion    = "before_save_completion"
	BarrierBeforeRestoreCompletion = "before_restore_completion"
	BarrierBeforeKVOStopping       = "before_kvo_stopping"
	BarrierBeforeKVOPausing        = "before_kvo_pausing"
	BarrierBeforeKVOSaving         = "before_kvo_saving"
	BarrierBeforeKVOResuming       = "before_kvo_resuming"
	BarrierBeforeKVORestoring      = "before_kvo_restoring"
)

// Domain and framework error names. Code 6 is the VM limit. Code 9 is
// operation cancelled, not the limit. See docs/vzcontract.md.
const (
	DomainVZ         = "VZErrorDomain"
	NameInvalidState = "VZErrorInvalidVirtualMachineState"
	NameLimit        = "VZErrorVirtualMachineLimitExceeded"
	NameRestore      = "VZErrorRestore"

	CodeInvalidVirtualMachineState  = 3
	CodeVirtualMachineLimitExceeded = 6
	CodeOperationCancelled          = 9
	CodeRestore                     = 12
)

// ErrInfo is the normalized completion error. Nil means success.
type ErrInfo struct {
	Domain string `json:"domain,omitempty"`
	Code   int    `json:"code,omitempty"`
	Name   string `json:"name,omitempty"`
	Errno  string `json:"errno,omitempty"`
}

// Event is one normalized transition. Kind is state_change,
// delegate_guest_did_stop, or completion_handler.
type Event struct {
	Kind  string   `json:"kind"`
	State string   `json:"state,omitempty"`
	Err   *ErrInfo `json:"err"`
}

func evState(state string) Event {
	return Event{Kind: "state_change", State: state}
}

func evGuest() Event {
	return Event{Kind: "delegate_guest_did_stop"}
}

func evOK() Event {
	return Event{Kind: "completion_handler"}
}

func evCode(code int, name string) Event {
	return Event{Kind: "completion_handler", Err: &ErrInfo{Domain: DomainVZ, Code: code, Name: name}}
}

func evErrno(errno string) Event {
	return Event{Kind: "completion_handler", Err: &ErrInfo{Errno: errno}}
}

func startEvents() []Event {
	return []Event{evState("Starting"), evState("Running"), evOK()}
}

func stopEvents(completionFirst bool) []Event {
	if completionFirst {
		return []Event{evState("Stopping"), evGuest(), evOK(), evState("Stopped")}
	}
	return []Event{evState("Stopping"), evGuest(), evState("Stopped"), evOK()}
}

func sameErr(a, b *ErrInfo) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameEvents(a, b []Event) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Kind != b[i].Kind || a[i].State != b[i].State || !sameErr(a[i].Err, b[i].Err) {
			return false
		}
	}
	return true
}

func eventsJSON(ev []Event) string {
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Sprint(ev)
	}
	return string(b)
}

func suffix(ev []Event, n int) []Event {
	if n > len(ev) {
		return ev
	}
	return ev[len(ev)-n:]
}

// errKey is the envelope token for a completion. "ok" is a nil error.
func errKey(e *ErrInfo) string {
	if e == nil {
		return "ok"
	}
	if e.Errno != "" {
		return "errno:" + e.Errno
	}
	if e.Domain != "" || e.Code != 0 || e.Name != "" {
		return fmt.Sprintf("vz:%s:%d:%s", e.Domain, e.Code, e.Name)
	}
	return "name:" + e.Name
}
