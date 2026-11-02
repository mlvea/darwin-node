package fake

import (
	"context"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/types"
)

func TestFakeLifecycle(t *testing.T) {
	r := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := r.Create(ctx, types.VMSpec{ID: types.MachineID{Name: "p", Namespace: "n", UID: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if st.State != types.VMRunning || !st.AgentOK || st.IP == "" {
		t.Fatalf("status %+v", st)
	}
	cli, err := m.DialAgent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	info, err := cli.NetInfo(ctx)
	if err != nil || info.PrimaryIP != st.IP {
		t.Fatalf("netinfo %+v %v", info, err)
	}
	pr, err := cli.Probe(ctx, guest.ProbeReq{Type: guest.ProbeExec, Argv: []string{"true"}})
	if err != nil || !pr.OK {
		t.Fatalf("probe %+v %v", pr, err)
	}
	if err := m.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if m.Status().State != types.VMStopped {
		t.Fatalf("state %s", m.Status().State)
	}
}

// The guest agent dials the host, serves one connection, and dials again
// only after that connection closes. Closing the first client must leave
// a later DialAgent able to reach the guest.
func TestDialAgentAfterCloseReachesGuest(t *testing.T) {
	r := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, err := r.Create(ctx, types.VMSpec{
		ID:         types.MachineID{Name: "p", Namespace: "n", UID: "u"},
		AgentToken: "tok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background(), 0) })

	first, err := m.DialAgent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := m.DialAgent(ctx)
	if err != nil {
		t.Fatalf("dial after close: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	info, err := second.NetInfo(ctx)
	if err != nil || info.PrimaryIP == "" {
		t.Fatalf("guest unreachable after the first session closed: %+v %v", info, err)
	}
}
