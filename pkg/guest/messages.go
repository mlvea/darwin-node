package guest

import "time"

// HandshakeReq is the first message on a connection.
// MinProtocol, MaxProtocol, and Capabilities are additive. An older host
// omits them. Unknown JSON fields are ignored by encoding/json.
type HandshakeReq struct {
	Token        string   `json:"token"`
	HostAgentVer string   `json:"hostAgentVer,omitempty"`
	TraceParent  string   `json:"traceparent,omitempty"`
	ExpectedOS   string   `json:"expectedOS,omitempty"`
	Nonce        string   `json:"nonce,omitempty"`
	MinProtocol  int      `json:"minProtocol,omitempty"`
	MaxProtocol  int      `json:"maxProtocol,omitempty"`
	Capabilities []string `json:"capabilities,omitempty"`
}

// HandshakeRes is the agent's identity.
type HandshakeRes struct {
	OK             bool   `json:"ok"`
	AgentVersion   string `json:"agentVersion"`
	Hostname       string `json:"hostname"`
	OSVersion      string `json:"osVersion"`
	Protocol       int    `json:"protocol"`
	MetalAvailable bool   `json:"metalAvailable"`
	// IdleMS is how long this agent waits with no frames before it closes
	// a non-interactive session. Zero means an older agent that uses the
	// 30s default and does not say so. The host heartbeats inside this window.
	IdleMS int `json:"idleMs,omitempty"`
	// StdinWindow is the initial host→guest stdin credit in payload bytes.
	// Zero means this agent does not grant credit. A new host then sends
	// stdin without waiting, which an older agent may drop when its queue fills.
	StdinWindow int `json:"stdinWindow,omitempty"`
	// Capabilities is the full set this agent implements. Empty means an
	// older agent. The host intersects it with its own list. Unknown
	// names are kept here and ignored by the intersection.
	Capabilities []string `json:"capabilities,omitempty"`
}

// HealthRes is agent liveness.
type HealthRes struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
	Uptime  string `json:"uptime,omitempty"`
}

// ReadyRes is workload readiness as the agent understands it
// (launchd jobs up, disks mounted). Kubernetes probes are separate.
type ReadyRes struct {
	Ready   bool   `json:"ready"`
	Message string `json:"message,omitempty"`
}

// ExecReq starts a command.
type ExecReq struct {
	Argv       []string          `json:"argv"`
	Env        map[string]string `json:"env,omitempty"`
	WorkingDir string            `json:"workingDir,omitempty"`
	TTY        bool              `json:"tty,omitempty"`
	Stdin      bool              `json:"stdin,omitempty"`
}

// ExecEvent is a stream frame for Exec.
type ExecEvent struct {
	Stdout   []byte `json:"stdout,omitempty"`
	Stderr   []byte `json:"stderr,omitempty"`
	StdinEOF bool   `json:"stdinEOF,omitempty"`
	Exited   bool   `json:"exited,omitempty"`
	ExitCode int    `json:"exitCode,omitempty"`
}

// ExecStdin is a client→agent stream frame carrying exec stdin. Sent on the
// same envelope ID as the Exec request until EOF.
type ExecStdin struct {
	Data []byte `json:"data,omitempty"`
	EOF  bool   `json:"eof,omitempty"`
}

// TtyResize is a client→agent stream frame resizing the exec PTY.
type TtyResize struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// LogsReq follows guest logs.
type LogsReq struct {
	Follow     bool      `json:"follow"`
	TailLines  int       `json:"tailLines,omitempty"`
	Since      time.Time `json:"since,omitempty"`
	Timestamps bool      `json:"timestamps,omitempty"`
}

// LogsEvent is one log chunk.
type LogsEvent struct {
	Line []byte `json:"line"`
}

// WindowUpdate grants the peer more stream credit for one envelope ID.
type WindowUpdate struct {
	Credit int `json:"credit"`
}

// ProbeType is a Kubernetes probe kind.
type ProbeType string

const (
	ProbeExec ProbeType = "exec"
	ProbeHTTP ProbeType = "httpGet"
	ProbeTCP  ProbeType = "tcpSocket"
)

// ProbeReq is executed inside the guest.
type ProbeReq struct {
	Type    ProbeType         `json:"type"`
	Argv    []string          `json:"argv,omitempty"`
	URL     string            `json:"url,omitempty"`
	Host    string            `json:"host,omitempty"`
	Port    int               `json:"port,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Timeout time.Duration     `json:"timeout"`
}

// ProbeRes is the probe outcome.
type ProbeRes struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"statusCode,omitempty"`
	Message    string `json:"message,omitempty"`
}

// MetricsRes is guest resource use.
type MetricsRes struct {
	CPUNanoCores     uint64 `json:"cpuNanoCores"`
	MemoryWorkingSet uint64 `json:"memoryWorkingSetBytes"`
	MemoryRSS        uint64 `json:"memoryRssBytes"`
	FsUsedBytes      uint64 `json:"fsUsedBytes"`
	FsCapBytes       uint64 `json:"fsCapBytes"`
	NetRxBytes       uint64 `json:"netRxBytes"`
	NetTxBytes       uint64 `json:"netTxBytes"`
	GPUNote          string `json:"gpuNote,omitempty"`
}

// NetInfoRes is guest addressing.
type NetInfoRes struct {
	PrimaryIP string   `json:"primaryIP"`
	IPs       []string `json:"ips"`
	MAC       string   `json:"mac,omitempty"`
	IFName    string   `json:"ifName,omitempty"`
}

// VolumePlace tells the agent how to expose a share at a pod mountPath.
type VolumePlace struct {
	Name      string `json:"name"`
	GuestPath string `json:"guestPath"`
	ReadOnly  bool   `json:"readOnly"`
	Mode      string `json:"mode"` // link | copy | bind
}

// MaterializeReq is sent after handshake.
type MaterializeReq struct {
	Volumes []VolumePlace `json:"volumes"`
}

// MaterializeRes reports placement.
type MaterializeRes struct {
	OK      bool     `json:"ok"`
	Placed  []string `json:"placed,omitempty"`
	Message string   `json:"message,omitempty"`
}

// WorkloadReq is the VM container's command, args, and env. The host
// sends it once the agent is up. An empty argv is not a request.
type WorkloadReq struct {
	Argv []string          `json:"argv,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	Dir  string            `json:"dir,omitempty"`
}

// WorkloadRes is the agent's acceptance of WorkloadReq. The process is
// started before the response; its exit is a later Exit notify.
type WorkloadRes struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}

// ShutdownReq asks the guest to halt.
type ShutdownReq struct {
	Reason  string `json:"reason,omitempty"`
	Timeout string `json:"timeout,omitempty"`
}

// SelftestReq runs an in-guest check (metal, disk, ...).
type SelftestReq struct {
	Name string `json:"name"`
}

// SelftestRes is the result.
type SelftestRes struct {
	OK      bool   `json:"ok"`
	Message string `json:"message,omitempty"`
}
