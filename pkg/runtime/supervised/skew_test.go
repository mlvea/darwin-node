package supervised

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"
)

type recordSkew struct {
	mu       sync.Mutex
	holds    []string
	releases []string
}

func (s *recordSkew) Hold(uid, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holds = append(s.holds, uid+" "+reason)
	return nil
}

func (s *recordSkew) Release(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases = append(s.releases, uid)
}

func (s *recordSkew) holdCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.holds)
}

func (s *recordSkew) releaseCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.releases)
}

func (s *recordSkew) reason() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.holds) == 0 {
		return ""
	}
	return s.holds[0]
}

func agentRuntime(dir string, skew supervisor.SkewControl) *Runtime {
	rt := New(Options{
		RunDir:        dir,
		Slots:         2,
		Deadman:       time.Hour,
		VMRuntime:     types.RuntimeFake,
		BinaryVersion: "agent-new",
	})
	rt.SetSkewControl(skew)
	return rt
}

func startConfigured(t *testing.T, dir string, opt func(*supervisor.Server)) *slotd {
	t.Helper()
	frt := fake.New()
	ctx, cancel := context.WithCancel(context.Background())
	srv := &supervisor.Server{
		Slot:          0,
		RunDir:        dir,
		RT:            frt,
		BinaryVersion: "sv-old",
		Deadman:       time.Hour,
	}
	if opt != nil {
		opt(srv)
	}
	s := &slotd{t: t, rt: frt, dir: dir, cancel: cancel, done: make(chan struct{})}
	var serveErr error
	go func() {
		serveErr = srv.Serve(ctx)
		close(s.done)
	}()
	path := supervisor.SocketPath(dir, 0)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-s.done:
			t.Fatalf("supervisor exited before the socket appeared: %v", serveErr)
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

func bootAndDrop(t *testing.T, rt *Runtime) {
	t.Helper()
	m, err := rt.Create(context.Background(), podSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.(*machine).dropConn()
}

func TestEqualMajorReadoptsDifferentSHA(t *testing.T) {
	dir := socketRunDir(t)
	rec := &recordSkew{}
	rt := agentRuntime(dir, rec)
	t.Cleanup(func() { rt.WaitSkew(3 * time.Second) })
	s := startConfigured(t, dir, func(srv *supervisor.Server) {
		srv.BuildSHA = "deadbeef"
	})
	bootAndDrop(t, agentRuntime(dir, nil))
	got, err := rt.Recover(context.Background(), []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-1", Found: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("recovered %d", len(got))
	}
	if rec.holdCount() != 0 {
		t.Fatalf("cordon on an equal major: %s", rec.reason())
	}
	hs := got[0].(*machine).VersionHandshake()
	if hs.BuildSHA != "deadbeef" || hs.WireMajor != supervisor.WireMajor || supervisor.Decide(supervisor.WireMajor, hs) != supervisor.ActionAdopt {
		t.Fatalf("handshake %+v", hs)
	}
	if s.rt.Running() != 1 {
		t.Fatalf("running %d", s.rt.Running())
	}
	if err := got[0].Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
}

func TestMinorAheadAndBehindStillAdopt(t *testing.T) {
	dir := socketRunDir(t)
	rt := agentRuntime(dir, &recordSkew{})
	t.Cleanup(func() { rt.WaitSkew(3 * time.Second) })
	startConfigured(t, dir, func(srv *supervisor.Server) {
		srv.AdvertiseWireMinor = 9
	})
	bootAndDrop(t, agentRuntime(dir, nil))
	got, err := rt.Recover(context.Background(), []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-1", Found: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("recovered %d", len(got))
	}
	hs := got[0].(*machine).VersionHandshake()
	if hs.WireMinor != 9 || !hs.Allows(supervisor.CapPause) || hs.Allows("not-yet") {
		t.Fatalf("handshake %+v", hs)
	}
	if err := got[0].Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}

	dir = socketRunDir(t)
	rt = agentRuntime(dir, &recordSkew{})
	t.Cleanup(func() { rt.WaitSkew(3 * time.Second) })
	startConfigured(t, dir, func(srv *supervisor.Server) {
		srv.SetCapabilities([]string{supervisor.CapHeartbeat})
	})
	bootAndDrop(t, agentRuntime(dir, nil))
	got, err = rt.Recover(context.Background(), []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-1", Found: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("recovered %d", len(got))
	}
	hs = got[0].(*machine).VersionHandshake()
	if hs.Allows(supervisor.CapPause) || !hs.Allows(supervisor.CapHeartbeat) {
		t.Fatalf("capabilities %+v", hs.Capabilities)
	}
	if err := got[0].Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
}

func TestWireMajorMismatchHoldsSlotUntilSupervisorExits(t *testing.T) {
	dir := socketRunDir(t)
	rec := &recordSkew{}
	rt := agentRuntime(dir, rec)
	t.Cleanup(func() { rt.WaitSkew(3 * time.Second) })
	s := startConfigured(t, dir, func(srv *supervisor.Server) {
		srv.AdvertiseWireMajor = 2
		srv.BuildSHA = "other"
	})
	bootAndDrop(t, agentRuntime(dir, nil))
	if s.rt.Running() != 1 {
		t.Fatalf("running %d before recover", s.rt.Running())
	}
	got, err := rt.Recover(context.Background(), []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-1", Found: true, Deleting: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("mismatch returned %d machines", len(got))
	}
	again, err := rt.Recover(context.Background(), []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-1", Found: true, Deleting: true,
	}})
	if err != nil || len(again) != 0 {
		t.Fatalf("second recover %d err %v", len(again), err)
	}
	if rec.holdCount() != 1 {
		t.Fatalf("holds %d (%s)", rec.holdCount(), rec.reason())
	}
	if rt.HeldSlots() != 1 {
		t.Fatalf("held slots %d", rt.HeldSlots())
	}
	time.Sleep(100 * time.Millisecond)
	if s.rt.Running() != 1 || s.rt.Starts() != 1 {
		t.Fatalf("mismatch stopped the VM: starts %d running %d", s.rt.Starts(), s.rt.Running())
	}
	if !strings.Contains(rec.reason(), "wire major 2") {
		t.Fatalf("reason %q", rec.reason())
	}
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not exit")
	}
	rt.WaitSkew(3 * time.Second)
	if rt.HeldSlots() != 0 {
		t.Fatalf("slot still held")
	}
	if rec.releaseCount() != 1 {
		t.Fatalf("releases %d", rec.releaseCount())
	}
}

func TestProtocolMajorMismatchHoldsWithoutStopping(t *testing.T) {
	dir := socketRunDir(t)
	rec := &recordSkew{}
	rt := agentRuntime(dir, rec)
	t.Cleanup(func() { rt.WaitSkew(3 * time.Second) })
	s := startConfigured(t, dir, func(srv *supervisor.Server) {
		srv.AdvertiseMajor = 99
	})
	lease := supervisor.Lease{
		PodUID:        "uid-legacy",
		PodName:       "pod",
		Namespace:     "ns",
		BootID:        "boot-1",
		SupervisorPID: os.Getpid(),
	}
	if err := supervisor.AnnotateProcess(&lease); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.WriteLease(dir, 0, lease); err != nil {
		t.Fatal(err)
	}
	got, err := rt.Recover(context.Background(), []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-legacy", Found: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || rt.HeldSlots() != 1 || rec.holdCount() != 1 {
		t.Fatalf("got %d held %d holds %d", len(got), rt.HeldSlots(), rec.holdCount())
	}
	if s.rt.Starts() != 0 {
		t.Fatalf("starts %d", s.rt.Starts())
	}
	if !strings.Contains(rec.reason(), "no version handshake") {
		t.Fatalf("reason %q", rec.reason())
	}
	time.Sleep(50 * time.Millisecond)
	if rt.HeldSlots() != 1 {
		t.Fatal("released while the supervisor was still listening")
	}
	s.cancel()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not exit")
	}
	rt.WaitSkew(3 * time.Second)
	if rt.HeldSlots() != 0 || rec.releaseCount() != 1 {
		t.Fatalf("held %d releases %d", rt.HeldSlots(), rec.releaseCount())
	}
}

func TestLegacySupervisorReleasesWhenItsProcessExits(t *testing.T) {
	dir := socketRunDir(t)
	rec := &recordSkew{}
	rt := agentRuntime(dir, rec)
	t.Cleanup(func() { rt.WaitSkew(3 * time.Second) })
	path := supervisor.SocketPath(dir, 0)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	var stops syncInt
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveLegacy(conn, &stops)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-acceptDone
	})
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	lease := supervisor.Lease{
		PodUID:        "uid-old",
		PodName:       "pod",
		Namespace:     "ns",
		BootID:        "boot-old",
		SupervisorPID: cmd.Process.Pid,
	}
	if err := supervisor.AnnotateProcess(&lease); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.WriteLease(dir, 0, lease); err != nil {
		t.Fatal(err)
	}
	got, err := rt.Recover(context.Background(), []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-old", Found: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 || rt.HeldSlots() != 1 || rec.holdCount() != 1 {
		t.Fatalf("got %d held %d holds %d reason %q", len(got), rt.HeldSlots(), rec.holdCount(), rec.reason())
	}
	time.Sleep(50 * time.Millisecond)
	if stops.n() != 0 {
		t.Fatalf("stop ops %d", stops.n())
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := cmd.Process.Wait(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal(err)
		}
	}
	cmd.Process = nil
	rt.WaitSkew(3 * time.Second)
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("socket gone before the process-exit release: %v", err)
	}
	if rt.HeldSlots() != 0 || rec.releaseCount() != 1 || stops.n() != 0 {
		t.Fatalf("held %d releases %d stops %d", rt.HeldSlots(), rec.releaseCount(), stops.n())
	}
}

func TestWorkloadStartedIsNotRunnableAgain(t *testing.T) {
	dir := socketRunDir(t)
	rt := agentRuntime(dir, &recordSkew{})
	t.Cleanup(func() { rt.WaitSkew(3 * time.Second) })
	startConfigured(t, dir, nil)
	first := agentRuntime(dir, nil)
	m, err := first.Create(context.Background(), podSpec())
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	code := int32(7)
	if err := m.(*machine).NoteWorkload(context.Background(), true, true, &code); err != nil {
		t.Fatal(err)
	}
	m.(*machine).dropConn()
	got, err := rt.Recover(context.Background(), []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-1", Found: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("recovered %d", len(got))
	}
	mm := got[0].(*machine)
	if !mm.WorkloadStarted() || supervisor.MayStartWorkload(mm.VersionHandshake()) {
		t.Fatal("workload can be started again")
	}
	gotCode, ok := mm.WorkloadExit()
	if !ok || gotCode != 7 {
		t.Fatalf("exit (%d, %v)", gotCode, ok)
	}
	if err := got[0].Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
}

type syncInt struct {
	mu sync.Mutex
	v  int
}

func (s *syncInt) add() {
	s.mu.Lock()
	s.v++
	s.mu.Unlock()
}

func (s *syncInt) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.v
}

func serveLegacy(conn net.Conn, stops *syncInt) {
	defer conn.Close()
	hello := []byte(`{"op":"hello","ok":true,"protocol_major":1,"protocol_minor":0,"binary_version":"sv-old","role":"supervisor","slot":0,"pid":4}`)
	if err := writeFrame(conn, hello); err != nil {
		return
	}
	if _, err := readFrame(conn); err != nil {
		return
	}
	if err := writeFrame(conn, []byte(`{"op":"ready","ok":true}`)); err != nil {
		return
	}
	for {
		body, err := readFrame(conn)
		if err != nil {
			return
		}
		var msg struct {
			Op string `json:"op"`
		}
		if json.Unmarshal(body, &msg) == nil && msg.Op == "stop_vm" {
			stops.add()
		}
		if err := writeFrame(conn, []byte(`{"op":"response","ok":true}`)); err != nil {
			return
		}
	}
}

func writeFrame(conn net.Conn, body []byte) error {
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(body)))
	copy(buf[4:], body)
	_, err := conn.Write(buf)
	return err
}

func readFrame(conn net.Conn) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	body := make([]byte, n)
	_, err := io.ReadFull(conn, body)
	return body, err
}
