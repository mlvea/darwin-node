package supervised

import (
	"context"
	"fmt"
	"os"

	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"
	"golang.org/x/sys/unix"
)

// adoptLive dials the supervisor that already has this slot's VM and
// attaches a new guest connection. The descriptor comes from that
// AttachChannels call. A fd saved from an earlier session is not reused.
func (m *machine) adoptLive(ctx context.Context, lease supervisor.Lease, pod supervisor.PodPresence) error {
	if m == nil || m.rt == nil {
		return fmt.Errorf("supervised runtime is nil")
	}
	sess, err := supervisor.Adopt(ctx, supervisor.AdoptRequest{
		Socket:        supervisor.SocketPath(m.rt.RunDir, m.slot),
		BinaryVersion: m.rt.version(),
		Slot:          m.slot,
		Lease:         lease,
		Pod:           pod,
	})
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.sess = sess
	m.mu.Unlock()
	if sess.Client() != nil {
		m.noteHandshake(sess.Client().Handshake())
	}
	m.armLost()
	return nil
}

// armLost releases the slot when the supervisor disappears. A voluntary
// close is SlotClosed and leaves the slot to Stop.
func (m *machine) armLost() {
	m.mu.Lock()
	sess := m.sess
	m.mu.Unlock()
	if sess == nil {
		return
	}
	go func() {
		if sess.WaitDone(context.Background()) == supervisor.SlotLost {
			m.noteLost()
		}
	}()
}

// noteLost marks the machine failed and releases the slot once. Stop
// still closes the control connection. It does not free the slot again,
// so a VM that took the slot after the loss keeps it.
func (m *machine) noteLost() {
	// Once waits: a status poll that arrives mid-release does not observe
	// the failed state until the slot has been given up.
	m.lostOnce.Do(func() {
		m.mu.Lock()
		if m.stopped || m.lostNoted {
			m.mu.Unlock()
			return
		}
		m.lostNoted = true
		st := m.status
		st.State = types.VMFailed
		st.Message = "supervisor lost"
		st.AgentOK = false
		m.status = st
		cancel := m.hbCancel
		sess := m.sess
		m.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if sess != nil {
			_ = sess.Close()
		}
		m.finishRelease()
	})
}

func (m *machine) finishRelease() {
	if m == nil || m.rt == nil {
		return
	}
	m.releaseOnce.Do(func() {
		_ = supervisor.RemoveLease(m.rt.RunDir, m.slot)
		m.rt.free(m.slot)
	})
}

func dupFD(f *os.File) (*os.File, error) {
	if f == nil {
		return nil, fmt.Errorf("nil guest fd")
	}
	raw, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	var nfd int
	var derr error
	if err := raw.Control(func(fd uintptr) {
		nfd, derr = unix.Dup(int(fd))
	}); err != nil {
		return nil, err
	}
	if derr != nil {
		return nil, derr
	}
	unix.CloseOnExec(nfd)
	out := os.NewFile(uintptr(nfd), f.Name())
	if out == nil {
		_ = unix.Close(nfd)
		return nil, fmt.Errorf("dup guest fd")
	}
	return out, nil
}
