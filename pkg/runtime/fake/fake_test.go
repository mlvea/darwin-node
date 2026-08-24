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
