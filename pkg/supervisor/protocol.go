// Package supervisor is the control protocol between the darwin-node
// agent and a per-VM supervisor process.
//
// A supervisor owns at most one virtual machine. The agent speaks five
// RPCs over a Unix socket: StartVM, InspectVM, PauseAndSaveVM, StopVM,
// and AttachChannels. AttachChannels is the only call that passes file
// descriptors. The guest-agent protocol is not spoken here.
package supervisor

import "errors"

const (
	// ProtocolMajor is the control protocol generation. The agent accepts
	// any supervisor binary of this major and refuses every other major,
	// so a running supervisor can keep its old binary until its VM exits.
	ProtocolMajor = 1
	// ProtocolMinor is this build's minor. Peers may differ.
	ProtocolMinor = 0
)

const (
	opHello     = "hello"
	opReady     = "ready"
	opStart     = "start_vm"
	opInspect   = "inspect_vm"
	opPause     = "pause_and_save_vm"
	opStop      = "stop_vm"
	opAttach    = "attach_channels"
	opHeartbeat = "heartbeat"
	opResponse  = "response"

	codeMajor = "protocol_major"
	codePeer  = "peer_uid"
	codeBusy  = "slot_busy"
	codeNoVM  = "no_vm"
	codePause = "pause_unsupported"
)

const maxFrame = 1 << 20

// Sentinel errors. Callers use errors.Is.
var (
	ErrProtocolMajor = errors.New("supervisor protocol major mismatch")
	ErrPeerUID       = errors.New("control socket peer uid rejected")
	ErrSlotBusy      = errors.New("supervisor slot is busy")
	ErrNoVM          = errors.New("supervisor has no vm")
	ErrFence         = errors.New("supervisor boot lease rejected")
)

// errPeerGone is ENOTCONN (or the peer disappearing before credentials
// could be read). The accept loop closes that connection and keeps
// listening. It is not an authentication failure.
var errPeerGone = errors.New("control socket peer disconnected")
