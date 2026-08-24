// Package types holds shared, Kubernetes-free domain types.
package types

import "time"

// AppleMaxConcurrentVMs is the hard Virtualization.framework / EULA cap.
const AppleMaxConcurrentVMs = 2

// Extended resource names advertised on the node.
const (
	ResourceVM    = "darwin.node/vm"
	ResourceMetal = "darwin.node/metal"
)

// Node labels.
const (
	LabelGPU            = "darwin.node/gpu"
	LabelGPUModel       = "darwin.node/gpu.model"
	LabelGPUMetal       = "darwin.node/gpu.metal"
	LabelGPUPassthrough = "darwin.node/gpu.passthrough"
	LabelNetworkMode    = "darwin.node/network-mode"
	LabelHostID         = "darwin.node/host-id"
	LabelRuntime        = "darwin.node/runtime"
	LabelCPUModel       = "feature.node.kubernetes.io/cpu-model.name"
)

// Taints.
const (
	TaintMacOSKey     = "darwin.node/macos"
	TaintMacOSValue   = "true"
	TaintNATKey       = "darwin.node/nat-only"
	TaintNATValue     = "true"
	TaintVMFullKey    = "darwin.node/vm-full"
	TaintVMFullValue  = "true"
	TaintProviderKey  = "virtual-kubelet.io/provider"
	TaintProviderVal  = "darwin-node"
)

// Well-known guest-agent ports.
const (
	GuestVsockPort = 50051
	GuestTCPPort   = 1050
)

// Shared directory names.
const (
	ControlShareName       = ".darwin-node"
	GuestAutomountRoot     = "/Volumes/My Shared Files"
	GuestAgentTokenFile    = "agent-token"
	GuestAgentLogPath      = "/var/log/darwin-guest-agent.log"
	GuestLaunchdLabel      = "io.darwin-node.guest-agent"
	HostLaunchdLabel       = "io.darwin-node.node"
	DefaultCacheBundleID   = "io.darwin-node.node"
)

// NetworkMode is the VM NIC attachment.
type NetworkMode string

const (
	NetworkNAT     NetworkMode = "nat"
	NetworkBridged NetworkMode = "bridged"
	NetworkDisabled NetworkMode = "disabled"
)

// RuntimeName selects the VM backend.
type RuntimeName string

const (
	RuntimeVZ   RuntimeName = "vz"
	RuntimeFake RuntimeName = "fake"
)

// Graphics is the paravirtual display used for Metal.
type Graphics struct {
	Enabled bool
	Width   int
	Height  int
	PPI     int
}

func DefaultGraphics() Graphics {
	return Graphics{Enabled: true, Width: 1920, Height: 1200, PPI: 80}
}

// VMState is the runtime-level machine state.
type VMState string

const (
	VMPending     VMState = "Pending"
	VMPreparing   VMState = "Preparing"
	VMStarting    VMState = "Starting"
	VMRunning     VMState = "Running"
	VMStopping    VMState = "Stopping"
	VMStopped     VMState = "Stopped"
	VMFailed      VMState = "Failed"
)

// AgentTransport is how the host talks to the guest agent.
type AgentTransport string

const (
	TransportVsock AgentTransport = "vsock"
	TransportTCP   AgentTransport = "tcp"
	TransportSSH   AgentTransport = "ssh"
	TransportLoop  AgentTransport = "loopback"
)

// MachineID identifies a pod's VM.
type MachineID struct {
	Namespace string
	Name      string
	UID       string
}

func (m MachineID) String() string {
	return m.Namespace + "/" + m.Name
}

// VMSpec is the runtime-level create request (no Pod types).
type VMSpec struct {
	ID            MachineID
	ImageRef      string
	CPU           uint
	MemoryBytes   uint64
	MAC           string
	NetworkMode   NetworkMode
	BridgeDevice  string
	Graphics      Graphics
	Shares        []Share
	AgentToken    string
	IgnoreCache   bool

	// Image-backed disk (required for the vz runtime).
	DiskPath              string
	AuxPath               string
	HardwareModelData     string // base64
	MachineIdentifierData string // base64; generated per pod if empty
}

// Share is a virtio-fs shared directory.
type Share struct {
	Name     string
	HostPath string
	ReadOnly bool
}

// VMStatus is a snapshot of a machine.
type VMStatus struct {
	State      VMState
	IP         string
	IPs        []string
	MAC        string
	StartedAt  *time.Time
	FinishedAt *time.Time
	AgentOK    bool
	Transport  AgentTransport
	Message    string
	Err        error
}
