package supervised

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/mlvea/darwin-node/pkg/supervisor"
)

// skewHold is one supervisor the node-agent refused to adopt. The slot
// stays reserved until that process leaves. finish runs once.
type skewHold struct {
	uid  string
	once sync.Once
	done chan struct{}
}

// SetSkewControl receives slot-hold and cordon calls for a wire mismatch.
// The engine implements supervisor.SkewControl. Nil clears it.
func (r *Runtime) SetSkewControl(c supervisor.SkewControl) {
	if r == nil {
		return
	}
	r.mu.Lock()
	r.skew = c
	r.mu.Unlock()
}

func (r *Runtime) skewBusy(slot int) bool {
	if r == nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.holds != nil && r.holds[slot] != nil
}

// considerSkew dials the slot and decides adopt versus hold. A hold keeps
// the connection so the supervisor's dead-man stays disarmed. An adopt
// closes this probe; adoptLive dials again. A protocol-major rejection
// cannot keep a connection, so the hold watches the pid and the socket
// and still does not call StopVM.
func (r *Runtime) considerSkew(ctx context.Context, slot int, lease supervisor.Lease) (bool, error) {
	c, err := r.dialSlot(ctx, slot)
	if err != nil {
		if errors.Is(err, supervisor.ErrProtocolMajor) {
			r.holdSkew(slot, lease, nil, supervisor.Handshake{Legacy: true})
			return true, nil
		}
		return false, err
	}
	hs := c.Handshake()
	if supervisor.Decide(supervisor.WireMajor, hs) == supervisor.ActionHold {
		r.holdSkew(slot, lease, c, hs)
		return true, nil
	}
	_ = c.Close()
	return false, nil
}

func (r *Runtime) holdSkew(slot int, lease supervisor.Lease, client *supervisor.Client, hs supervisor.Handshake) {
	uid := lease.PodUID
	if uid == "" {
		uid = fmt.Sprintf("supervisor-slot-%d", slot)
	}
	r.mu.Lock()
	if r.holds == nil {
		r.holds = map[int]*skewHold{}
	}
	if r.holds[slot] != nil {
		r.mu.Unlock()
		if client != nil {
			_ = client.Close()
		}
		return
	}
	h := &skewHold{uid: uid, done: make(chan struct{})}
	r.holds[slot] = h
	skew := r.skew
	r.mu.Unlock()
	r.mark(slot)
	reason := fmt.Sprintf("%s; slot %d", supervisor.HoldReason(supervisor.WireMajor, hs), slot)
	if skew != nil {
		_ = skew.Hold(uid, reason)
	}
	go r.watchSkew(slot, lease, client, h)
}

// watchSkew heartbeats while the supervisor is alive. The socket
// disappearing is how an in-process supervisor is observed to exit:
// shutdown removes it only after the VM has been stopped. A separate
// process is observed through the lease pid. An application-level
// heartbeat error leaves the connection open so the dead-man stays
// disarmed. Only a dead connection is redialed, and at most once a
// second.
func (r *Runtime) watchSkew(slot int, lease supervisor.Lease, client *supervisor.Client, h *skewHold) {
	defer func() {
		if client != nil {
			_ = client.Close()
		}
		r.finishSkew(slot, h)
	}()
	sock := supervisor.SocketPath(r.RunDir, slot)
	var lastDial time.Time
	var lastBeat time.Time
	for {
		if supervisorExited(lease, sock) {
			return
		}
		now := time.Now()
		if client == nil && now.Sub(lastDial) >= time.Second {
			lastDial = now
			if _, err := os.Stat(sock); err == nil {
				c, err := r.dialSlot(context.Background(), slot)
				if err == nil {
					if supervisor.Decide(supervisor.WireMajor, c.Handshake()) != supervisor.ActionHold {
						client = c
						return
					}
					client = c
				}
			}
		}
		if client != nil && now.Sub(lastBeat) >= time.Second {
			lastBeat = now
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			err := client.Heartbeat(ctx)
			cancel()
			if err != nil && networkDown(err) {
				_ = client.Close()
				client = nil
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (r *Runtime) finishSkew(slot int, h *skewHold) {
	if r == nil || h == nil {
		return
	}
	h.once.Do(func() {
		r.mu.Lock()
		if r.holds != nil && r.holds[slot] == h {
			delete(r.holds, slot)
		}
		skew := r.skew
		r.mu.Unlock()
		r.free(slot)
		if skew != nil {
			skew.Release(h.uid)
		}
		close(h.done)
	})
}

// WaitSkew blocks until every hold has released, or until d elapses.
// Tests register it after the supervisor cleanup so a hold goroutine
// is gone before the leak checker runs.
func (r *Runtime) WaitSkew(d time.Duration) {
	if r == nil {
		return
	}
	r.mu.Lock()
	waits := make([]chan struct{}, 0, len(r.holds))
	for _, h := range r.holds {
		if h != nil {
			waits = append(waits, h.done)
		}
	}
	r.mu.Unlock()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for _, ch := range waits {
		select {
		case <-ch:
		case <-timer.C:
			return
		}
	}
}

func supervisorExited(lease supervisor.Lease, sock string) bool {
	if lease.SupervisorPID > 0 && lease.HostBootID != "" && lease.SupervisorProcStart != 0 {
		err := supervisor.CheckProcessLease(lease)
		if err != nil && (errors.Is(err, supervisor.ErrFence) || errors.Is(err, supervisor.ErrProcGone)) {
			return true
		}
	}
	if sock == "" {
		return false
	}
	_, err := os.Stat(sock)
	return errors.Is(err, os.ErrNotExist)
}

func networkDown(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
		return true
	}
	var op *net.OpError
	if errors.As(err, &op) && !op.Timeout() {
		return true
	}
	return false
}

func (m *machine) noteHandshake(hs supervisor.Handshake) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.hs = hs
	if hs.WorkloadStarted {
		m.workloadStarted = true
	}
	if hs.WorkloadExited {
		m.workloadExited = true
		if hs.ExitCode != nil {
			code := *hs.ExitCode
			m.workloadCode = &code
		}
	}
}

// VersionHandshake is the snapshot taken when this machine was adopted.
func (m *machine) VersionHandshake() supervisor.Handshake {
	if m == nil {
		return supervisor.Handshake{Legacy: true}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hs
}

// WorkloadStarted reports that the guest workload already ran, so a
// recovered pod must not call Workload again.
func (m *machine) WorkloadStarted() bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workloadStarted
}

// WorkloadExit is the guest code when the handshake or a later note
// said the workload finished.
func (m *machine) WorkloadExit() (int32, bool) {
	if m == nil {
		return 0, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.workloadExited && m.workloadCode != nil {
		return *m.workloadCode, true
	}
	return 0, false
}

// NoteWorkload tells the supervisor the guest workload started or
// exited. Local flags are set even when the RPC fails, and a started
// workload stays started.
func (m *machine) NoteWorkload(ctx context.Context, started, exited bool, code *int32) error {
	if m == nil {
		return fmt.Errorf("supervised machine is nil")
	}
	m.mu.Lock()
	client := m.client
	if client == nil && m.sess != nil {
		client = m.sess.Client()
	}
	if started {
		m.workloadStarted = true
	}
	if exited {
		m.workloadExited = true
		if code != nil {
			c := *code
			m.workloadCode = &c
		}
	}
	m.mu.Unlock()
	if client == nil {
		return fmt.Errorf("supervisor connection is closed")
	}
	var noteCode *int32
	if code != nil {
		c := *code
		noteCode = &c
	}
	return client.NoteWorkload(ctx, supervisor.WorkloadNote{Started: started, Exited: exited, ExitCode: noteCode})
}
