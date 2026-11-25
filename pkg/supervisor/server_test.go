package supervisor

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/types"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

type harness struct {
	t      *testing.T
	rt     *fake.Runtime
	dir    string
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// socketRunDir is a short directory. macOS sun_path holds 103 bytes, and
// t.TempDir under /var/folders plus a long test name does not fit.
func socketRunDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func newHarness(t *testing.T, dead time.Duration, opt func(*Server)) *harness {
	t.Helper()
	dir := socketRunDir(t)
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	rt := fake.New()
	ctx, cancel := context.WithCancel(context.Background())
	srv := &Server{
		Slot:          0,
		RunDir:        dir,
		RT:            rt,
		BinaryVersion: "sv-old",
		Deadman:       dead,
	}
	if opt != nil {
		opt(srv)
	}
	h := &harness{t: t, rt: rt, dir: dir, cancel: cancel, done: make(chan struct{})}
	go func() {
		h.err = srv.Serve(ctx)
		close(h.done)
	}()
	t.Cleanup(h.stop)
	h.waitReady()
	return h
}

func (h *harness) waitReady() {
	h.t.Helper()
	path := SocketPath(h.dir, 0)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-h.done:
			h.t.Fatalf("server exited early: %v", h.err)
		default:
		}
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("socket %s did not appear", path)
}

func (h *harness) stop() {
	h.cancel()
	select {
	case <-h.done:
	case <-time.After(5 * time.Second):
		h.t.Errorf("supervisor did not exit")
	}
}

func (h *harness) dial() *Client {
	h.t.Helper()
	c, err := Dial(context.Background(), SocketPath(h.dir, 0), "agent-new", 0)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { _ = c.Close() })
	return c
}

func spec() types.VMSpec {
	return types.VMSpec{
		ID:       types.MachineID{Namespace: "ns", Name: "pod", UID: "uid-1"},
		ImageRef: "sha256:abc",
		CPU:      2,
	}
}

func TestHandshakeAcceptsDifferentMinorAndBinary(t *testing.T) {
	h := newHarness(t, time.Hour, func(s *Server) { s.AdvertiseMinor = 7 })
	fi, err := os.Stat(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("run dir mode %#o, want 0700", fi.Mode().Perm())
	}
	sock, err := os.Stat(SocketPath(h.dir, 0))
	if err != nil {
		t.Fatal(err)
	}
	if sock.Mode().Perm() != 0o600 {
		t.Fatalf("socket mode %#o, want 0600", sock.Mode().Perm())
	}
	c := h.dial()
	major, minor, binary := c.PeerVersion()
	if major != ProtocolMajor || minor != 7 || binary != "sv-old" {
		t.Fatalf("peer version %d.%d %s", major, minor, binary)
	}
}

func TestHandshakeRejectsUnknownMajor(t *testing.T) {
	h := newHarness(t, time.Hour, func(s *Server) { s.AdvertiseMajor = 99 })
	_, err := Dial(context.Background(), SocketPath(h.dir, 0), "agent-new", 0)
	if !errors.Is(err, ErrProtocolMajor) {
		t.Fatalf("dial error %v, want protocol major mismatch", err)
	}
	if h.rt.Starts() != 0 {
		t.Fatalf("rejected handshake started %d vms", h.rt.Starts())
	}
}

func TestServerRejectsClientMajor(t *testing.T) {
	h := newHarness(t, time.Hour, nil)
	_, err := dialMajor(context.Background(), SocketPath(h.dir, 0), "agent-new", 0, 99)
	if !errors.Is(err, ErrProtocolMajor) {
		t.Fatalf("dial error %v, want protocol major mismatch", err)
	}
	if _, err := Dial(context.Background(), SocketPath(h.dir, 0), "agent-new", 0); err != nil {
		t.Fatalf("a later matching client was refused: %v", err)
	}
}

func TestRPCsDescriptorsAndIdempotentStart(t *testing.T) {
	before := fdCount()
	t.Cleanup(func() {
		if got := fdCount(); got != before {
			t.Errorf("fd leak: before %d after %d\n%s", before, got, strings.Join(fdList(), "\n"))
		}
	})
	h := newHarness(t, time.Hour, nil)
	ctx := context.Background()
	c := h.dial()
	insp, err := c.StartVM(ctx, StartRequest{Spec: spec(), BootID: "boot-1", HMACToken: "token-1", ImageDigest: "sha256:abc"})
	if err != nil {
		t.Fatal(err)
	}
	if insp.BootID != "boot-1" || insp.HMACToken != "token-1" || insp.ImageDigest != "sha256:abc" {
		t.Fatalf("inspect %+v", insp)
	}
	if insp.State != string(types.VMRunning) || insp.PodUID != "uid-1" || insp.PID != os.Getpid() {
		t.Fatalf("inspect %+v pid %d", insp, os.Getpid())
	}
	if h.rt.Starts() != 1 || h.rt.Running() != 1 {
		t.Fatalf("starts %d running %d", h.rt.Starts(), h.rt.Running())
	}
	again, err := c.StartVM(ctx, StartRequest{Spec: spec(), BootID: "boot-1", HMACToken: "token-1", ImageDigest: "sha256:abc"})
	if err != nil {
		t.Fatal(err)
	}
	if again.BootID != "boot-1" || h.rt.Starts() != 1 {
		t.Fatalf("second start created a vm: starts %d boot %s", h.rt.Starts(), again.BootID)
	}
	_, err = c.StartVM(ctx, StartRequest{Spec: spec(), BootID: "boot-2", HMACToken: "token-1", ImageDigest: "sha256:abc"})
	if !errors.Is(err, ErrSlotBusy) {
		t.Fatalf("different boot: %v", err)
	}

	got, err := c.InspectVM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.BootID != "boot-1" || got.BinaryVersion != "sv-old" || got.ProtocolMajor != ProtocolMajor {
		t.Fatalf("inspect %+v", got)
	}
	if err := c.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}

	save := filepath.Join(h.dir, "vm.sav")
	if err := c.PauseAndSaveVM(ctx, save); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(save)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "darwin-node pause\n" {
		t.Fatalf("save body %q", body)
	}
	paused, err := c.InspectVM(ctx)
	if err != nil || !paused.Paused {
		t.Fatalf("paused %+v err %v", paused, err)
	}

	netFD, agentFD, err := c.AttachChannels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = netFD.Close() }()
	defer func() { _ = agentFD.Close() }()
	netPeer, agentPeer, err := h.rt.ChannelPeers("uid-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = netPeer.Close() }()
	defer func() { _ = agentPeer.Close() }()
	if _, err := netFD.Write([]byte("nic-frame")); err != nil {
		t.Fatal(err)
	}
	if got := readOnce(t, netPeer); string(got) != "nic-frame" {
		t.Fatalf("nic peer read %q", got)
	}
	if _, err := agentFD.Write([]byte("agent-bytes")); err != nil {
		t.Fatal(err)
	}
	if got := readFull(t, agentPeer, len("agent-bytes")); string(got) != "agent-bytes" {
		t.Fatalf("agent peer read %q", got)
	}

	if err := c.StopVM(ctx, time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor did not exit after stop")
	}
	if h.rt.Running() != 0 {
		t.Fatalf("running %d", h.rt.Running())
	}
}

func TestReattachKeepsTheVM(t *testing.T) {
	h := newHarness(t, time.Hour, nil)
	ctx := context.Background()
	c := h.dial()
	if _, err := c.StartVM(ctx, StartRequest{Spec: spec(), BootID: "boot-1", HMACToken: "token-1", ImageDigest: "sha256:abc"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2 := h.dial()
	got, err := c2.InspectVM(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.BootID != "boot-1" || got.HMACToken != "token-1" || got.State != string(types.VMRunning) {
		t.Fatalf("reattach inspect %+v", got)
	}
	if h.rt.Running() != 1 || h.rt.Starts() != 1 {
		t.Fatalf("starts %d running %d", h.rt.Starts(), h.rt.Running())
	}
}

func TestLeaseMismatchStopsVM(t *testing.T) {
	h := newHarness(t, time.Hour, nil)
	ctx := context.Background()
	c := h.dial()
	insp, err := c.StartVM(ctx, StartRequest{Spec: spec(), BootID: "boot-1", HMACToken: "token-1", ImageDigest: "sha256:abc"})
	if err != nil {
		t.Fatal(err)
	}
	lease := Lease{
		PodUID:                insp.PodUID,
		PodName:               insp.PodName,
		Namespace:             insp.Namespace,
		BootID:                "boot-other",
		ImageDigest:           insp.ImageDigest,
		SupervisorPID:         insp.PID,
		SupervisorStartTimeNS: insp.StartTimeNS,
		HMACToken:             insp.HMACToken,
	}
	if err := Fence(lease, insp, PodPresence{Found: true, UID: insp.PodUID}); !errors.Is(err, ErrFence) {
		t.Fatalf("fence %v", err)
	}
	if err := c.StopVM(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if h.rt.Running() != 0 {
		t.Fatalf("running %d after lease mismatch stop", h.rt.Running())
	}
}

func TestPeerUIDRejected(t *testing.T) {
	h := newHarness(t, time.Hour, func(s *Server) {
		s.AllowUID = -1
		s.AllowUIDSet = true
	})
	_, err := Dial(context.Background(), SocketPath(h.dir, 0), "agent-new", 0)
	if !errors.Is(err, ErrPeerUID) {
		t.Fatalf("dial error %v, want peer uid rejection", err)
	}
	if h.rt.Starts() != 0 {
		t.Fatalf("rejected peer started %d vms", h.rt.Starts())
	}
}

func TestDeadmanStopsVM(t *testing.T) {
	const dead = 200 * time.Millisecond
	h := newHarness(t, dead, nil)
	ctx := context.Background()
	c := h.dial()
	if _, err := c.StartVM(ctx, StartRequest{Spec: spec(), BootID: "boot-1", HMACToken: "token-1", ImageDigest: "sha256:abc"}); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.done:
		if time.Since(started) < dead/2 {
			t.Fatalf("deadman fired after %s, want at least %s", time.Since(started), dead/2)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("deadman did not fire")
	}
	if h.err != nil {
		t.Fatal(h.err)
	}
	if h.rt.Running() != 0 {
		t.Fatalf("running %d", h.rt.Running())
	}
}

func TestDeadmanHoldsWhileConnectedAndResetsOnHeartbeat(t *testing.T) {
	const dead = 200 * time.Millisecond
	h := newHarness(t, dead, nil)
	ctx := context.Background()
	c := h.dial()
	if _, err := c.StartVM(ctx, StartRequest{Spec: spec(), BootID: "boot-1", HMACToken: "token-1", ImageDigest: "sha256:abc"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(dead + 100*time.Millisecond)
	select {
	case <-h.done:
		t.Fatal("deadman fired while the agent was connected")
	default:
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(dead / 3)
	c2 := h.dial()
	if err := c2.Heartbeat(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c2.Close(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(dead / 3)
	select {
	case <-h.done:
		t.Fatal("deadman fired before the reset interval elapsed")
	default:
	}
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		t.Fatal("deadman did not fire after the reset disconnect")
	}
	if h.rt.Running() != 0 {
		t.Fatalf("running %d", h.rt.Running())
	}
}

func TestRunDirSymlinkRejected(t *testing.T) {
	// The path check runs before the symlink check. t.TempDir on macOS
	// already overflows sun_path once slot-0.sock is appended.
	dir := socketRunDir(t)
	real := filepath.Join(dir, "real")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	srv := &Server{Slot: 0, RunDir: link, RT: fake.New(), Deadman: time.Hour}
	err := srv.Serve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink run dir error %v", err)
	}
}

func TestSocketPathTooLong(t *testing.T) {
	dir := strings.Repeat("p", maxSocketPathLen())
	srv := &Server{Slot: 0, RunDir: dir, RT: fake.New(), Deadman: time.Hour}
	err := srv.Serve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "socket path") {
		t.Fatalf("got %v", err)
	}
}

func readOnce(t *testing.T, f *os.File) []byte {
	t.Helper()
	if err := f.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	n, err := f.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n]
}

func readFull(t *testing.T, f *os.File, n int) []byte {
	t.Helper()
	if err := f.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		t.Fatal(err)
	}
	return buf
}

func fdCount() int {
	if runtime.GOOS != "linux" {
		return 0
	}
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	return len(ents)
}

func fdList() []string {
	if runtime.GOOS != "linux" {
		return nil
	}
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return []string{err.Error()}
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		target, _ := os.Readlink("/proc/self/fd/" + e.Name())
		out = append(out, e.Name()+":"+target)
	}
	return out
}
