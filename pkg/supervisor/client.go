package supervisor

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/types"
)

// Client is one agent connection to a slot supervisor.
type Client struct {
	conn net.Conn
	raw  *net.UnixConn
	mu   sync.Mutex

	peerMajor int
	peerMinor int
	peerBin   string
	slot      int
}

// StartRequest is StartVM. BootID and HMACToken are the fence the agent
// writes into the lease. The supervisor stores them and echoes them from
// InspectVM. It does not derive them.
type StartRequest struct {
	Spec        types.VMSpec
	BootID      string
	HMACToken   string
	ImageDigest string
}

// Dial handshakes with the supervisor listening on path. expectSlot is
// checked against the supervisor hello when it is zero or positive.
// A negative expectSlot skips that check.
func Dial(ctx context.Context, path, binaryVersion string, expectSlot int) (*Client, error) {
	return dialMajor(ctx, path, binaryVersion, expectSlot, ProtocolMajor)
}

func dialMajor(ctx context.Context, path, binaryVersion string, expectSlot, major int) (*Client, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	uc, ok := c.(*net.UnixConn)
	if !ok {
		_ = c.Close()
		return nil, fmt.Errorf("supervisor socket is not a unix connection")
	}
	if err := setDeadline(c, ctx); err != nil {
		_ = c.Close()
		return nil, err
	}
	defer func() { _ = c.SetDeadline(time.Time{}) }()

	hello, err := readMsg(c)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if hello.Op == opResponse && !hello.OK {
		_ = c.Close()
		return nil, mapErr(hello)
	}
	if hello.Op != opHello || hello.Role != "supervisor" {
		_ = c.Close()
		return nil, fmt.Errorf("expected supervisor hello, got %q", hello.Op)
	}
	if hello.ProtocolMajor != ProtocolMajor {
		_ = c.Close()
		return nil, fmt.Errorf("%w: supervisor speaks %d.%d (%s), this agent speaks %d",
			ErrProtocolMajor, hello.ProtocolMajor, hello.ProtocolMinor, hello.BinaryVersion, ProtocolMajor)
	}
	if expectSlot >= 0 && hello.Slot != expectSlot {
		_ = c.Close()
		return nil, fmt.Errorf("supervisor slot %d, want %d", hello.Slot, expectSlot)
	}
	ours := message{
		Op:            opHello,
		ProtocolMajor: major,
		ProtocolMinor: ProtocolMinor,
		BinaryVersion: binaryVersion,
		Role:          "agent",
		Slot:          hello.Slot,
	}
	if err := writeMsg(c, ours); err != nil {
		_ = c.Close()
		return nil, err
	}
	ack, err := readMsg(c)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if !ack.OK {
		_ = c.Close()
		return nil, mapErr(ack)
	}
	return &Client{
		conn:      c,
		raw:       uc,
		peerMajor: hello.ProtocolMajor,
		peerMinor: hello.ProtocolMinor,
		peerBin:   hello.BinaryVersion,
		slot:      hello.Slot,
	}, nil
}

// PeerVersion is the supervisor hello. Minor and binary may differ from
// this process. Major matches ProtocolMajor or Dial would have failed.
func (c *Client) PeerVersion() (major, minor int, binary string) {
	return c.peerMajor, c.peerMinor, c.peerBin
}

// Slot is the slot the supervisor reported.
func (c *Client) Slot() int { return c.slot }

// StartVM creates and starts the slot's only VM.
func (c *Client) StartVM(ctx context.Context, req StartRequest) (Inspect, error) {
	resp, err := c.call(ctx, message{
		Op:          opStart,
		Spec:        &req.Spec,
		BootID:      req.BootID,
		HMACToken:   req.HMACToken,
		ImageDigest: req.ImageDigest,
	})
	if err != nil {
		return Inspect{}, err
	}
	return inspectFrom(resp), nil
}

// InspectVM reports the VM without changing it.
func (c *Client) InspectVM(ctx context.Context) (Inspect, error) {
	resp, err := c.call(ctx, message{Op: opInspect})
	if err != nil {
		return Inspect{}, err
	}
	return inspectFrom(resp), nil
}

// PauseAndSaveVM asks the supervisor to pause the VM and write path.
func (c *Client) PauseAndSaveVM(ctx context.Context, path string) error {
	_, err := c.call(ctx, message{Op: opPause, SavePath: path})
	return err
}

// StopVM stops the VM. The supervisor process then exits.
func (c *Client) StopVM(ctx context.Context, grace time.Duration) error {
	_, err := c.call(ctx, message{Op: opStop, GraceMS: grace.Milliseconds()})
	return err
}

// Heartbeat refreshes the supervisor's last-seen time. An open connection
// already holds the dead-man switch; this is the explicit heartbeat.
func (c *Client) Heartbeat(ctx context.Context) error {
	_, err := c.call(ctx, message{Op: opHeartbeat})
	return err
}

// AttachChannels returns the NIC datagram and the connected guest-agent
// stream. The caller owns both files. This process does not read them
// before returning; the supervisor does not parse the guest protocol.
func (c *Client) AttachChannels(ctx context.Context) (netFD, agentFD *os.File, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := setDeadline(c.conn, ctx); err != nil {
		return nil, nil, err
	}
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()

	req := message{Op: opAttach, ID: newMsgID()}
	if err := writeMsg(c.conn, req); err != nil {
		return nil, nil, err
	}
	resp, err := readMsg(c.conn)
	if err != nil {
		return nil, nil, err
	}
	if resp.ID != req.ID {
		return nil, nil, fmt.Errorf("response id %q, want %q", resp.ID, req.ID)
	}
	if !resp.OK {
		return nil, nil, mapErr(resp)
	}
	if resp.FDCount != 2 {
		return nil, nil, fmt.Errorf("attach_channels fd_count %d, want 2", resp.FDCount)
	}
	files, err := recvFDs(c.raw, 2)
	if err != nil {
		return nil, nil, err
	}
	return files[0], files[1], nil
}

// Close drops the control connection. The VM keeps running until StopVM,
// the dead-man switch, or the supervisor process exits.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return nil
	}
	err := c.conn.Close()
	c.conn = nil
	return err
}

func (c *Client) call(ctx context.Context, req message) (message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return message{}, fmt.Errorf("supervisor connection is closed")
	}
	if err := setDeadline(c.conn, ctx); err != nil {
		return message{}, err
	}
	defer func() { _ = c.conn.SetDeadline(time.Time{}) }()
	req.ID = newMsgID()
	if err := writeMsg(c.conn, req); err != nil {
		return message{}, err
	}
	resp, err := readMsg(c.conn)
	if err != nil {
		return message{}, err
	}
	if resp.ID != req.ID {
		return resp, fmt.Errorf("response id %q, want %q", resp.ID, req.ID)
	}
	if resp.Op != opResponse {
		return resp, fmt.Errorf("response op %q", resp.Op)
	}
	if !resp.OK {
		return resp, mapErr(resp)
	}
	return resp, nil
}

func setDeadline(conn net.Conn, ctx context.Context) error {
	if ctx == nil {
		return conn.SetDeadline(time.Time{})
	}
	if dl, ok := ctx.Deadline(); ok {
		return conn.SetDeadline(dl)
	}
	return conn.SetDeadline(time.Time{})
}

func newMsgID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func mapErr(msg message) error {
	text := msg.Err
	if text == "" {
		text = "supervisor request failed"
	}
	switch msg.Code {
	case codeMajor:
		return fmt.Errorf("%w: %s", ErrProtocolMajor, text)
	case codePeer:
		return fmt.Errorf("%w: %s", ErrPeerUID, text)
	case codeBusy:
		return fmt.Errorf("%w: %s", ErrSlotBusy, text)
	case codeNoVM:
		return fmt.Errorf("%w: %s", ErrNoVM, text)
	case codePause:
		return fmt.Errorf("%s", text)
	default:
		return errors.New(text)
	}
}
