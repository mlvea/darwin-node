package supervisor

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Lease is the boot fence the agent writes under the run directory.
// On reattach, InspectVM must report the same boot id, token, process
// identity, and image. A pod that is gone or deleting fails the fence.
type Lease struct {
	PodUID                string `json:"pod_uid"`
	PodName               string `json:"pod_name"`
	Namespace             string `json:"namespace"`
	BootID                string `json:"boot_id"`
	ImageDigest           string `json:"image_digest"`
	SupervisorPID         int    `json:"supervisor_pid"`
	SupervisorStartTimeNS int64  `json:"supervisor_start_time_ns"`
	HMACToken             string `json:"hmac_token"`
	// HostBootID is the host boot session (kern.bootsessionuuid, or
	// Linux boot_id). It is not the VM boot UUID in BootID.
	HostBootID string `json:"host_boot_id,omitempty"`
	// SupervisorProcStart is the OS process start time of SupervisorPID.
	// Linux stores /proc/<pid>/stat starttime ticks. Darwin stores
	// kinfo_proc start time in nanoseconds.
	SupervisorProcStart int64 `json:"supervisor_proc_start,omitempty"`
}

// PodPresence is what the agent knows about the pod from the API server.
// Found is false when the pod is gone. Deleting is true when
// DeletionTimestamp is set.
type PodPresence struct {
	Namespace string
	Name      string
	UID       string
	Found     bool
	Deleting  bool
}

// Inspect is the supervisor's report of the VM it owns.
type Inspect struct {
	BootID        string
	HMACToken     string
	ImageDigest   string
	Namespace     string
	PodName       string
	PodUID        string
	PID           int
	StartTimeNS   int64
	BinaryVersion string
	ProtocolMajor int
	ProtocolMinor int
	State         string
	IP            string
	MAC           string
	AgentOK       bool
	Message       string
	ExitCode      *int32
	Paused        bool
}

// SocketPath is <run dir>/slot-N.sock.
func SocketPath(runDir string, slot int) string {
	return filepath.Join(runDir, fmt.Sprintf("slot-%d.sock", slot))
}

// LeasePath is <run dir>/slots/slot-N/lease.json.
func LeasePath(runDir string, slot int) string {
	return filepath.Join(runDir, "slots", fmt.Sprintf("slot-%d", slot), "lease.json")
}

// WriteLease atomically replaces the slot lease. The slot directory and
// its parent are mode 0700. The file is mode 0600.
func WriteLease(runDir string, slot int, lease Lease) error {
	final := LeasePath(runDir, slot)
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Dir(dir), 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := final + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, final)
}

// ReadLease loads the slot lease.
func ReadLease(runDir string, slot int) (Lease, error) {
	body, err := os.ReadFile(LeasePath(runDir, slot))
	if err != nil {
		return Lease{}, err
	}
	var lease Lease
	if err := json.Unmarshal(body, &lease); err != nil {
		return Lease{}, err
	}
	return lease, nil
}

// RemoveLease deletes the slot lease. A missing file is not an error.
func RemoveLease(runDir string, slot int) error {
	err := os.Remove(LeasePath(runDir, slot))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Fence reports whether the running VM may be kept. A non-nil error means
// the agent must StopVM and delete the lease.
func Fence(lease Lease, got Inspect, pod PodPresence) error {
	if got.BootID != lease.BootID {
		return fmt.Errorf("%w: boot id", ErrFence)
	}
	if !tokenEqual(got.HMACToken, lease.HMACToken) {
		return fmt.Errorf("%w: token", ErrFence)
	}
	if got.PID != lease.SupervisorPID || got.StartTimeNS != lease.SupervisorStartTimeNS {
		return fmt.Errorf("%w: supervisor identity", ErrFence)
	}
	if got.PodUID != lease.PodUID || got.Namespace != lease.Namespace || got.PodName != lease.PodName {
		return fmt.Errorf("%w: pod identity", ErrFence)
	}
	if got.ImageDigest != lease.ImageDigest {
		return fmt.Errorf("%w: image digest", ErrFence)
	}
	if !pod.Found || pod.Deleting {
		return fmt.Errorf("%w: pod is gone", ErrFence)
	}
	if pod.UID != "" && pod.UID != lease.PodUID {
		return fmt.Errorf("%w: pod uid", ErrFence)
	}
	return nil
}

// NewBootID is a random UUID version 4.
func NewBootID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// NewToken is an HMAC-SHA256 of identity under a random key. The key is
// discarded. The token itself is the secret stored in the lease and in
// the supervisor.
func NewToken(identity string) (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(identity))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

func tokenEqual(a, b string) bool {
	ab, aerr := hex.DecodeString(a)
	bb, berr := hex.DecodeString(b)
	if aerr == nil && berr == nil && len(ab) > 0 && len(ab) == len(bb) {
		return hmac.Equal(ab, bb)
	}
	if len(a) != len(b) {
		return false
	}
	return hmac.Equal([]byte(a), []byte(b))
}

func inspectFrom(msg message) Inspect {
	return Inspect{
		BootID:        msg.BootID,
		HMACToken:     msg.HMACToken,
		ImageDigest:   msg.ImageDigest,
		Namespace:     msg.Namespace,
		PodName:       msg.PodName,
		PodUID:        msg.PodUID,
		PID:           msg.PID,
		StartTimeNS:   msg.StartTimeNS,
		BinaryVersion: msg.BinaryVersion,
		ProtocolMajor: msg.ProtocolMajor,
		ProtocolMinor: msg.ProtocolMinor,
		State:         msg.State,
		IP:            msg.IP,
		MAC:           msg.MAC,
		AgentOK:       msg.AgentOK,
		Message:       msg.Message,
		ExitCode:      msg.ExitCode,
		Paused:        msg.Paused,
	}
}
