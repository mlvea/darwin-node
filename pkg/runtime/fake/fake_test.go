package fake

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/types"
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

// Stop has to end every in-process Serve, including dials whose clients
// the host never closed. Those watchdogs otherwise stay in select until
// the idle timeout, which is longer than leakcheck waits.
func TestStopEndsGuestWatchdogs(t *testing.T) {
	leakcheck.Check(t)
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
	var held []*guest.Client
	for i := 0; i < 3; i++ {
		cli, err := m.DialAgent(ctx)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, cli)
	}
	if err := m.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	// Keep the clients reachable so their heartbeats, not Stop, are what
	// would have held the guest connections open.
	runtime.KeepAlive(held)
}

// Stop has to join every guest Serve before it returns. Close used to
// cancel the context and return while idleWatchdog was still blocked in
// select; Darwin CI then reported that goroutine as a leak.
func TestStopReturnsAfterGuestServeExits(t *testing.T) {
	leakcheck.Check(t)
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
	// Start already holds one guest connection. Stay under the shared limit.
	var clients []*guest.Client
	for i := 0; i < 2; i++ {
		cli, err := m.DialAgent(ctx)
		if err != nil {
			t.Fatal(err)
		}
		clients = append(clients, cli)
	}
	t.Cleanup(func() {
		for _, cli := range clients {
			_ = cli.Close()
		}
	})
	if err := m.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	if strings.Contains(string(buf[:n]), "idleWatchdog") {
		t.Fatal("idleWatchdog still running after Stop returned")
	}
	if _, err := m.DialAgent(ctx); err == nil {
		t.Fatal("DialAgent after Stop started another guest")
	}
	for _, cli := range clients {
		_ = cli.Close()
	}
}
