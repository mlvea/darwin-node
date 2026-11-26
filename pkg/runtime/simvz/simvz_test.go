package simvz

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/types"
	"github.com/mlvea/darwin-node/pkg/vzcontract"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	if vzcontract.ChildEnabled() {
		if err := vzcontract.ServeChild(New(Options{Dir: os.Getenv("DARWIN_NODE_SUPERVISOR_RUN_DIR")})); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	goleak.VerifyTestMain(m)
}

func spec(name string) types.VMSpec {
	return types.VMSpec{
		ID:         types.MachineID{Namespace: "default", Name: name, UID: name},
		AgentToken: "tok",
		MAC:        "02:00:00:00:00:11",
	}
}

func newRT(t *testing.T, opts Options) *Runtime {
	t.Helper()
	if opts.Dir == "" {
		opts.Dir = t.TempDir()
	}
	return New(opts)
}

func mustMachine(t *testing.T, rt *Runtime, name string) *machine {
	t.Helper()
	m, err := rt.Create(context.Background(), spec(name))
	if err != nil {
		t.Fatal(err)
	}
	mm, ok := m.(*machine)
	if !ok {
		t.Fatalf("%T", m)
	}
	t.Cleanup(func() {
		_ = mm.Stop(context.Background(), 0)
		mm.shutdown()
	})
	return mm
}

func fdBefore(t *testing.T) int {
	t.Helper()
	if _, err := FDCount(); err != nil {
		t.Fatal(err)
	}
	n, err := FDCount()
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func fdBack(t *testing.T, before int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var last int
	for {
		n, err := FDCount()
		if err != nil {
			t.Fatal(err)
		}
		last = n
		if n <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("open fds %d, baseline %d", last, before)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLifecycleDialAndDatagram(t *testing.T) {
	before := fdBefore(t)
	rt := newRT(t, Options{})
	if rt.Name() != Name {
		t.Fatalf("name %s", rt.Name())
	}
	m := mustMachine(t, rt, "a")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if m.State() != "Running" || m.Status().State != types.VMRunning {
		t.Fatalf("state %s status %s", m.State(), m.Status().State)
	}
	if rt.Running() != 1 {
		t.Fatalf("running %d", rt.Running())
	}
	host, guestEnd := m.NetworkFiles()
	if host == nil || guestEnd == nil {
		t.Fatal("nic socketpair was not opened")
	}
	if _, err := host.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = guestEnd.SetReadDeadline(time.Now().Add(time.Second))
	buf := make([]byte, 8)
	n, err := guestEnd.Read(buf)
	if err != nil || string(buf[:n]) != "ping" {
		t.Fatalf("datagram %q %v", buf[:n], err)
	}
	st, err := osStat(m.DiskPath())
	if err != nil || st == 0 {
		t.Fatalf("disk %s size %d err %v", m.DiskPath(), st, err)
	}
	cli, err := m.DialAgent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	info, err := cli.NetInfo(ctx)
	if err != nil || info.PrimaryIP != m.Status().IP {
		t.Fatalf("netinfo %+v %v ip %s", info, err, m.Status().IP)
	}
	ready, err := cli.Ready(ctx)
	if err != nil || !ready.Ready {
		t.Fatalf("ready %+v %v", ready, err)
	}
	if err := cli.Close(); err != nil {
		t.Fatal(err)
	}
	logs := m.Logs()
	body, err := io.ReadAll(logs)
	_ = logs.Close()
	if err != nil || len(body) == 0 {
		t.Fatalf("logs %q %v", body, err)
	}
	if err := m.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if m.State() != "Stopped" || rt.Running() != 0 {
		t.Fatalf("after stop %s running %d", m.State(), rt.Running())
	}
	if err := m.Stop(ctx, 0); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("second stop %v", err)
	}
	fdBack(t, before)
}

func TestDialAfterClose(t *testing.T) {
	before := fdBefore(t)
	rt := newRT(t, Options{})
	m := mustMachine(t, rt, "redial")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
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
	if _, err := second.Health(ctx); err != nil {
		t.Fatal(err)
	}
	_ = second.Close()
	_ = m.Stop(ctx, 0)
	fdBack(t, before)
}

func TestThirdVMRejectedAndSlotReturns(t *testing.T) {
	before := fdBefore(t)
	rt := newRT(t, Options{})
	ctx := context.Background()
	ms := make([]*machine, 3)
	for i, name := range []string{"a", "b", "c"} {
		ms[i] = mustMachine(t, rt, name)
	}
	start := make(chan struct{})
	errs := make([]error, 3)
	done := make(chan struct{})
	for i := range ms {
		go func(i int) {
			<-start
			errs[i] = ms[i].Start(ctx)
			done <- struct{}{}
		}(i)
	}
	close(start)
	for range ms {
		<-done
	}
	var failed int
	for _, err := range errs {
		if err == nil {
			continue
		}
		failed++
		if !errors.Is(err, ErrVirtualMachineLimitExceeded) {
			t.Fatalf("err %v", err)
		}
		if !strings.Contains(err.Error(), "VZErrorVirtualMachineLimitExceeded") {
			t.Fatalf("text %v", err)
		}
	}
	if failed != 1 || rt.Running() != 2 {
		t.Fatalf("failed %d running %d errs %v", failed, rt.Running(), errs)
	}
	for i, err := range errs {
		if err == nil {
			if stopErr := ms[i].Stop(ctx, 0); stopErr != nil {
				t.Fatal(stopErr)
			}
		}
	}
	if rt.Running() != 0 {
		t.Fatalf("running %d", rt.Running())
	}
	var loser *machine
	for i, err := range errs {
		if err != nil {
			loser = ms[i]
		}
	}
	if loser == nil {
		t.Fatal("no VM was rejected")
	}
	if err := loser.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if rt.Running() != 1 {
		t.Fatalf("running %d", rt.Running())
	}
	_ = loser.Stop(ctx, 0)
	fdBack(t, before)
}

func TestMaxRunningClampedAtTwo(t *testing.T) {
	rt := newRT(t, Options{MaxRunning: 5})
	if rt.max != 2 {
		t.Fatalf("max %d", rt.max)
	}
	rt = newRT(t, Options{MaxRunning: 1})
	ctx := context.Background()
	a := mustMachine(t, rt, "one")
	b := mustMachine(t, rt, "two")
	if err := a.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(ctx); !errors.Is(err, ErrVirtualMachineLimitExceeded) {
		t.Fatalf("second %v", err)
	}
}

func TestDialRequiresRunning(t *testing.T) {
	rt := newRT(t, Options{})
	m := mustMachine(t, rt, "down")
	_, err := m.DialAgent(context.Background())
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("%v", err)
	}
}

func osStat(path string) (int64, error) {
	if path == "" {
		return 0, errors.New("empty disk path")
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

func TestGuestBinaryHandshake(t *testing.T) {
	bin, err := GuestAgentBinary(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	before := fdBefore(t)
	rt := newRT(t, Options{GuestBinary: bin})
	m := mustMachine(t, rt, "child")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cli, err := m.DialAgent(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := cli.Ready(ctx)
	if err != nil || !ready.Ready {
		t.Fatalf("ready %+v %v", ready, err)
	}
	// The child reports the host's own address. The handshake is the
	// contract under test; the simulated NIC address is the in-process path.
	if _, err := cli.Health(ctx); err != nil {
		t.Fatal(err)
	}
	_ = cli.Close()
	if err := m.Stop(ctx, 0); err != nil {
		t.Fatal(err)
	}
	fdBack(t, before)
}
