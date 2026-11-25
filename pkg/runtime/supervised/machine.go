package supervised

import (
	"context"
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
	netFD  *os.File

	mu       sync.Mutex
	stopped  bool
	hbCancel context.CancelFunc
	status   types.VMStatus
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
	if err := supervisor.WriteLease(m.rt.RunDir, m.slot, lease); err != nil {
		_ = m.client.StopVM(context.Background(), 0)
		_ = m.client.Close()
		return err
	}
	m.mu.Lock()
	m.status = statusFrom(insp)
	m.mu.Unlock()
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
	m.netFD = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if netFD != nil {
		_ = netFD.Close()
	}
	var err error
	if m.client != nil {
		err = m.client.StopVM(ctx, grace)
		_ = m.client.Close()
	}
	if m.rt != nil {
		_ = supervisor.RemoveLease(m.rt.RunDir, m.slot)
		m.rt.free(m.slot)
	}
	m.mu.Lock()
	m.status.State = types.VMStopped
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
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if m.client != nil {
		_ = m.client.Close()
	}
}

func (m *machine) Status() types.VMStatus {
	m.mu.Lock()
	c := m.client
	stopped := m.stopped
	cached := m.status
	m.mu.Unlock()
	if c == nil || stopped {
		return cached
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	insp, err := c.InspectVM(ctx)
	if err != nil {
		return cached
	}
	st := statusFrom(insp)
	m.mu.Lock()
	m.status = st
	m.mu.Unlock()
	return st
}

func (m *machine) DialAgent(ctx context.Context) (*guest.Client, error) {
	netFD, agentFD, err := m.client.AttachChannels(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.netFD != nil {
		_ = m.netFD.Close()
	}
	m.netFD = netFD
	token := m.spec.AgentToken
	ver := m.rt.version()
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
				cctx, ccancel := context.WithTimeout(context.Background(), interval)
				_ = m.client.Heartbeat(cctx)
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
