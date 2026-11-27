package supervisor

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"

	"github.com/mlvea/darwin-node/pkg/types"
)

// message is one length-prefixed JSON frame. Zero values that are
// meaningful on the wire (slot 0, protocol minor 0, ok false) are not
// tagged omitempty.
type message struct {
	Op  string `json:"op"`
	ID  string `json:"id,omitempty"`
	OK  bool   `json:"ok"`
	Err string `json:"error,omitempty"`
	// Code is a stable errors.Is key. Text in Err is for logs.
	Code string `json:"code,omitempty"`

	ProtocolMajor int    `json:"protocol_major"`
	ProtocolMinor int    `json:"protocol_minor"`
	BinaryVersion string `json:"binary_version,omitempty"`
	Role          string `json:"role,omitempty"`
	Slot          int    `json:"slot"`

	// Version-skew handshake. wire_major is a pointer so a supervisor
	// that predates these fields (the key is absent) is distinct from
	// an explicit 0. Unknown JSON keys are ignored by encoding/json.
	WireMajor       *int     `json:"wire_major,omitempty"`
	WireMinor       int      `json:"wire_minor,omitempty"`
	BuildSHA        string   `json:"build_sha,omitempty"`
	Capabilities    []string `json:"capabilities,omitempty"`
	HostBootID      string   `json:"host_boot_id,omitempty"`
	WorkloadStarted bool     `json:"workload_started,omitempty"`
	WorkloadExited  bool     `json:"workload_exited,omitempty"`
	PID             int      `json:"pid,omitempty"`
	StartTimeNS     int64    `json:"start_time_ns,omitempty"`

	Spec        *types.VMSpec `json:"spec,omitempty"`
	BootID      string        `json:"boot_id,omitempty"`
	HMACToken   string        `json:"hmac_token,omitempty"`
	ImageDigest string        `json:"image_digest,omitempty"`
	Namespace   string        `json:"namespace,omitempty"`
	PodName     string        `json:"pod_name,omitempty"`
	PodUID      string        `json:"pod_uid,omitempty"`

	SavePath string `json:"save_path,omitempty"`
	GraceMS  int64  `json:"grace_ms,omitempty"`
	Paused   bool   `json:"paused,omitempty"`

	State    string `json:"state,omitempty"`
	IP       string `json:"ip,omitempty"`
	MAC      string `json:"mac,omitempty"`
	AgentOK  bool   `json:"agent_ok,omitempty"`
	Message  string `json:"message,omitempty"`
	ExitCode *int32 `json:"exit_code,omitempty"`

	// FDCount tells the peer that this frame is followed by one carrier
	// byte with that many SCM_RIGHTS descriptors. The descriptors are
	// not part of the JSON.
	FDCount int `json:"fd_count,omitempty"`
}

func writeMsg(conn net.Conn, msg message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	if len(body) > maxFrame {
		return fmt.Errorf("supervisor frame is %d bytes", len(body))
	}
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(body)))
	copy(buf[4:], body)
	_, err = conn.Write(buf)
	return err
}

func readMsg(conn net.Conn) (message, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return message{}, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrame {
		return message{}, fmt.Errorf("supervisor frame is %d bytes", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(conn, body); err != nil {
		return message{}, err
	}
	var msg message
	if err := json.Unmarshal(body, &msg); err != nil {
		return message{}, err
	}
	return msg, nil
}

func errMsg(code, text string) message {
	return message{Op: opResponse, OK: false, Code: code, Err: text}
}
