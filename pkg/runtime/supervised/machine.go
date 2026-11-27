package supervised

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"
)

type machine struct {
	rt     *Runtime
	id     types.MachineID
	spec   types.VMSpec
	slot   int
	client *supervisor.Client
	sess   *supervisor.Session
	netFD  *os.File

	mu          sync.Mutex
	stopped     bool
	lostNoted   bool
	lostOnce    sync.Once
	releaseOnce sync.Once
	hbCancel    context.CancelFunc
	status      types.VMStatus

	supPID    int
	procStart int64
	hostBoot  string

	hs              supervisor.Handshake
	workloadStarted bool
	workloadExited  bool
	workloadCode    *int32
}

func (m *machine) ID() types.MachineID { return m.id }

func (m *machine) Start(ctx context.Context) error {
	boot, err := supervisor.NewBootID()
	if err != nil {
		return err
	}
	digest := m.spec.ImageRef
	token, err := supervisor.NewToken(boot + "\x00" + m.spec.ID.UID + "\x00" + digest)
	if err != nil {
		return err
	}
	insp, err := m.client.StartVM(ctx, supervisor.StartRequest{
		Spec:        m.spec,
		BootID:      boot,
		HMACToken:   token,
		ImageDigest: digest,
	})
	if err != nil {
		return err
	}
	lease := supervisor.Lease{
		PodUID:                insp.PodUID,
		PodName:               insp.PodName,
		Namespace:             insp.Namespace,
		BootID:                insp.BootID,
		ImageDigest:           insp.ImageDigest,
		SupervisorPID:         insp.PID,
		SupervisorStartTimeNS: insp.StartTimeNS,
		HMACToken:             insp.HMACToken,
	}
	if err := supervisor.AnnotateProcess(&lease); err != nil {
		_ = m.client.StopVM(context.Background(), 0)
		_ = m.client.Close()
		return err
	}
	m.mu.Lock()
	m.supPID = lease.SupervisorPID
	m.procStart = lease.SupervisorProcStart
	m.hostBoot = lease.HostBootID
	m.mu.Unlock()
	if err := supervisor.WriteLease(m.rt.RunDir, m.slot, lease); err != nil {
		_ = m.client.StopVM(context.Background(), 0)
		_ = m.client.Close()
		return err
	}
	pod := supervisor.PodPresence{
		Namespace: insp.Namespace,
		Name:      insp.PodName,
		UID:       insp.PodUID,
		Found:     true,
	}
	if err := m.adoptLive(ctx, lease, pod); err != nil {
		_ = m.client.StopVM(context.Background(), 0)
		_ = m.client.Close()
		_ = supervisor.RemoveLease(m.rt.RunDir, m.slot)
		return err
	}
	m.mu.Lock()
	lost := m.lostNoted
	if !lost {
		m.status = statusFrom(insp)
	}
	m.mu.Unlock()
	if lost {
		return fmt.Errorf("supervisor lost")
	}
	m.startHeartbeat()
	return nil
}

func (m *machine) Stop(ctx context.Context, grace time.Duration) error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	m.stopped = true
	cancel := m.hbCancel
	netFD := m.netFD
	client := m.client
	sess := m.sess
	ident := supervisor.ProcessIdentity{PID: m.supPID, Start: m.procStart, BootID: m.hostBoot}
	m.netFD = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if netFD != nil {
		_ = netFD.Close()
	}
	var err error
	// Start keeps its own control connection. Recover's control
	// connection is a second dial. The session closes only the
	// connection Adopt opened.
	sessionClient := sess != nil && client == sess.Client()
	if client != nil {
		err = enforceStop(ctx, agentStopBudget(grace, 0), ident, func(ctx context.Context) error {
			return client.StopVM(ctx, grace)
		})
	}
	if sess != nil {
		if cerr := sess.Close(); err == nil {
			err = cerr
		}
	}
	if client != nil && !sessionClient {
		if cerr := client.Close(); err == nil {
			err = cerr
		}
	}
	m.finishRelease()
	m.mu.Lock()
	if !m.lostNoted {
		m.status.State = types.VMStopped
	}
	m.mu.Unlock()
	return err
}

// dropConn simulates the agent process dying. The VM keeps running, the
// slot stays reserved, and a later Recover adopts it. A second Stop on
// this machine does not free the slot out from under that recovery.
func (m *machine) dropConn() {
	m.mu.Lock()
	m.stopped = true
	cancel := m.hbCancel
	client := m.client
	sess := m.sess
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if client != nil && (sess == nil || client != sess.Client()) {
		_ = client.Close()
	}
	if sess != nil {
		_ = sess.Close()
	}
}

func (m *machine) Status() types.VMStatus {
	st := m.rawStatus()
	m.mu.Lock()
	exited := m.workloadExited
	code := m.workloadCode
	m.mu.Unlock()
	if exited && code != nil {
		c := *code
		st.ExitCode = &c
	}
	return st
}

func (m *machine) rawStatus() types.VMStatus {
	m.mu.Lock()
	c := m.client
	sess := m.sess
	stopped := m.stopped
	lost := m.lostNoted
	cached := m.status
	m.mu.Unlock()
	if !stopped && (lost || (sess != nil && sess.State() == supervisor.SlotLost)) {
		m.noteLost()
		m.mu.Lock()
		st := m.status
		m.mu.Unlock()
		return st
	}
	if c == nil || stopped {
		return cached
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	insp, err := c.InspectVM(ctx)
	if err != nil {
		if sess != nil && sess.State() == supervisor.SlotLost {
			m.noteLost()
			m.mu.Lock()
			st := m.status
			m.mu.Unlock()
			return st
		}
		return cached
	}
	st := statusFrom(insp)
	m.mu.Lock()
	if m.lostNoted {
		st = m.status
	} else {
		m.status = st
	}
	m.mu.Unlock()
	return st
}

func (m *machine) DialAgent(ctx context.Context) (*guest.Client, error) {
	m.mu.Lock()
	sess := m.sess
	client := m.client
	token := m.spec.AgentToken
	ver := ""
	if m.rt != nil {
		ver = m.rt.version()
	}
	m.mu.Unlock()
	if sess != nil {
		src := sess.Guest()
		dup, err := dupFD(src)
		if err != nil {
			return nil, err
		}
		cli, err := guest.Dial(ctx, dup, token, ver)
		if err != nil {
			_ = dup.Close()
			return nil, err
		}
		return cli, nil
	}
	if client == nil {
		return nil, fmt.Errorf("supervisor connection is closed")
	}
	netFD, agentFD, err := client.AttachChannels(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.netFD != nil {
		_ = m.netFD.Close()
	}
	m.netFD = netFD
	m.mu.Unlock()
	return guest.Dial(ctx, agentFD, token, ver)
}

func (m *machine) Logs() io.ReadCloser {
	return io.NopCloser(strings.NewReader("supervised runtime: use the guest agent for logs\n"))
}

func (m *machine) startHeartbeat() {
	interval := m.rt.Deadman / 3
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	if interval > time.Minute {
		interval = time.Minute
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.hbCancel = cancel
	m.mu.Unlock()
	go func() {
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				m.mu.Lock()
				c := m.client
				m.mu.Unlock()
				if c == nil {
					continue
				}
				cctx, ccancel := context.WithTimeout(context.Background(), interval)
				_ = c.Heartbeat(cctx)
				ccancel()
			}
		}
	}()
}

func statusFrom(insp supervisor.Inspect) types.VMStatus {
	st := types.VMStatus{
		State:    types.VMState(insp.State),
		IP:       insp.IP,
		MAC:      insp.MAC,
		AgentOK:  insp.AgentOK,
		Message:  insp.Message,
		ExitCode: insp.ExitCode,
	}
	if st.State == "" {
		st.State = types.VMPending
	}
	return st
}
