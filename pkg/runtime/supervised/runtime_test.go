package supervised

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type slotd struct {
	t      *testing.T
	rt     *fake.Runtime
	dir    string
	cancel context.CancelFunc
	done   chan struct{}
}

func startSlot(t *testing.T, bridge bool) *slotd {
	t.Helper()
	dir := t.TempDir()
	rt := fake.New()
	rt.SetBridgeGuest(bridge)
	ctx, cancel := context.WithCancel(context.Background())
	srv := &supervisor.Server{
		Slot:          0,
		RunDir:        dir,
		RT:            rt,
		BinaryVersion: "sv-old",
		Deadman:       time.Hour,
	}
	s := &slotd{t: t, rt: rt, dir: dir, cancel: cancel, done: make(chan struct{})}
	go func() {
		_ = srv.Serve(ctx)
		close(s.done)
	}()
	path := supervisor.SocketPath(dir, 0)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			t.Fatal("supervisor exited before the socket appeared")
		default:
		}
		if _, err := os.Stat(path); err == nil {
			t.Cleanup(s.stop)
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	t.Fatalf("socket %s did not appear", path)
	return nil
}

func (s *slotd) stop() {
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		s.t.Errorf("supervisor did not exit")
	}
}

func (s *slotd) dial(ctx context.Context, slot int) (*supervisor.Client, error) {
	if slot != 0 {
		return nil, fmt.Errorf("no supervisor for slot %d", slot)
	}
	return supervisor.Dial(ctx, supervisor.SocketPath(s.dir, 0), "agent-new", 0)
}

func (s *slotd) adapter(slots int) *Runtime {
	return New(Options{
		RunDir:        s.dir,
		Slots:         slots,
		Deadman:       time.Hour,
		VMRuntime:     types.RuntimeFake,
		BinaryVersion: "agent-new",
		Dial:          s.dial,
	})
}

func podSpec() types.VMSpec {
	return types.VMSpec{
		ID:         types.MachineID{Namespace: "ns", Name: "pod", UID: "uid-1"},
		ImageRef:   "sha256:abc",
		CPU:        2,
		AgentToken: "tok",
	}
}

func TestNameAndNoFreeSlot(t *testing.T) {
	if New(Options{}).Name() != types.RuntimeVZ {
		t.Fatal("empty runtime name")
	}
	s := startSlot(t, false)
	rt := s.adapter(1)
	if rt.Name() != types.RuntimeFake {
		t.Fatalf("name %s", rt.Name())
	}
	ctx := context.Background()
	m, err := rt.Create(ctx, podSpec())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background(), 0) })
	if _, err := rt.Create(ctx, podSpec()); err == nil || !strings.Contains(err.Error(), "no free supervisor slot") {
		t.Fatalf("second create: %v", err)
	}
	logs, err := io.ReadAll(m.Logs())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(logs), "guest agent") {
		t.Fatalf("logs %q", logs)
	}
}

func TestStartStatusAndDialAgent(t *testing.T) {
	s := startSlot(t, true)
	rt := s.adapter(1)
	ctx := context.Background()
	m, err := rt.Create(ctx, podSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if st.State != types.VMRunning {
		t.Fatalf("status %+v", st)
	}
	if s.rt.Running() != 1 || s.rt.Starts() != 1 {
		t.Fatalf("starts %d running %d", s.rt.Starts(), s.rt.Running())
	}
	lease, err := supervisor.ReadLease(s.dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if lease.BootID == "" || lease.PodUID != "uid-1" || lease.HMACToken == "" {
		t.Fatalf("lease %+v", lease)
	}
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cli, err := m.DialAgent(dctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	if s.rt.Running() != 0 {
		t.Fatalf("running %d", s.rt.Running())
	}
}

func TestRecoverKeepsTheVM(t *testing.T) {
	s := startSlot(t, false)
	ctx := context.Background()
	first := s.adapter(1)
	m, err := first.Create(ctx, podSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	m.(*machine).dropConn()
	second := s.adapter(1)
	got, err := second.Recover(ctx, []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-1", Found: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("recovered %d", len(got))
	}
	if s.rt.Running() != 1 || s.rt.Starts() != 1 {
		t.Fatalf("starts %d running %d", s.rt.Starts(), s.rt.Running())
	}
	if got[0].Status().State != types.VMRunning {
		t.Fatalf("status %+v", got[0].Status())
	}
	if err := got[0].Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if s.rt.Running() != 0 {
		t.Fatalf("running %d", s.rt.Running())
	}
}

func TestRecoverStopsOnLeaseMismatch(t *testing.T) {
	s := startSlot(t, false)
	ctx := context.Background()
	rt := s.adapter(1)
	m, err := rt.Create(ctx, podSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	lease, err := supervisor.ReadLease(s.dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	lease.BootID = "boot-other"
	if err := supervisor.WriteLease(s.dir, 0, lease); err != nil {
		t.Fatal(err)
	}
	m.(*machine).dropConn()
	got, err := s.adapter(1).Recover(ctx, []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-1", Found: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("recovered %d after a bad lease", len(got))
	}
	if s.rt.Running() != 0 {
		t.Fatalf("running %d", s.rt.Running())
	}
}

func TestRecoverStopsADeletingPod(t *testing.T) {
	s := startSlot(t, false)
	ctx := context.Background()
	m, err := s.adapter(1).Create(ctx, podSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	m.(*machine).dropConn()
	got, err := s.adapter(1).Recover(ctx, []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-1", Found: true, Deleting: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || s.rt.Running() != 0 {
		t.Fatalf("recovered %d running %d", len(got), s.rt.Running())
	}
}

func TestSpawnSetsid(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("session id is read from /proc")
	}
	p, err := spawnProcess("/bin/sleep", []string{"30"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Kill)
	sid, err := sessionID(p.cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if sid != p.cmd.Process.Pid {
		t.Fatalf("session %d, pid %d", sid, p.cmd.Process.Pid)
	}
}

func TestLaunchdSpawnIsDarwinOnly(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("launchctl is not invoked from this test")
	}
	err := kickstart(0)
	if err == nil {
		t.Fatal("expected launchd to be refused")
	}
	if !strings.Contains(err.Error(), "darwin") {
		t.Fatal(err)
	}
}

func sessionID(pid int) (int, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	i := bytes.LastIndexByte(b, ')')
	if i < 0 || i+1 >= len(b) {
		return 0, errors.New("proc stat has no command field")
	}
	fields := strings.Fields(string(b[i+1:]))
	if len(fields) < 4 {
		return 0, fmt.Errorf("proc stat %q", b)
	}
	return strconv.Atoi(fields[3])
}
