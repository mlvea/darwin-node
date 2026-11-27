package vzcontract

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"
	"go.uber.org/goleak"
	"golang.org/x/sys/unix"
)

// testRestart locks the per-VM supervisor handoff.
//
// agent_restart: the supervisor keeps the VM. A new node-agent adopts it
// and must receive a new Connect dial, not a dup of the previous guest fd.
// supervisor_killed: the supervisor process is SIGKILLed. The living
// node-agent must observe EOF or EPIPE on the guest fd, mark the slot
// lost, and release every fd and goroutine it held.
func testRestart(t *testing.T, factory Factory) {
	t.Run("agent_restart", func(t *testing.T) { testAgentRestart(t, factory) })
	t.Run("supervisor_killed", func(t *testing.T) { testSupervisorKilled(t, factory) })
}

func testAgentRestart(t *testing.T, factory Factory) {
	leaks := goleak.IgnoreCurrent()
	prepare(t)
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })

	dir := shortRunDir(t)
	br := newRestartBridge(t, factory)
	stop := startInProcSupervisor(t, dir, br)
	t.Cleanup(stop)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	insp := bootVM(t, ctx, dir)
	lease := leaseFrom(t, insp)
	if err := supervisor.WriteLease(dir, 0, lease); err != nil {
		t.Fatal(err)
	}
	req := adoptReq(dir, lease, insp, "agent-old")

	sess1, err := supervisor.Adopt(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess1.Close() })
	if !sess1.Held() || sess1.State() != supervisor.SlotRunning {
		t.Fatalf("state %s held %v", sess1.State(), sess1.Held())
	}
	if sess1.Status().State != types.VMRunning {
		t.Fatalf("status %+v", sess1.Status())
	}
	if sess1.Slot() != 0 {
		t.Fatalf("slot %d", sess1.Slot())
	}
	guest1 := sess1.Guest()
	if guest1 == nil {
		t.Fatal("no guest fd")
	}
	assertSockType(t, guest1, unix.SOCK_STREAM)
	kept, err := dupOSFile(guest1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kept.Close() })
	ino1, err := fileIno(kept)
	if err != nil {
		t.Fatal(err)
	}

	if err := sess1.Close(); err != nil {
		t.Fatal(err)
	}
	if sess1.State() != supervisor.SlotClosed || sess1.Held() {
		t.Fatalf("agent close marked the slot %s held %v", sess1.State(), sess1.Held())
	}

	req.BinaryVersion = "agent-new"
	sess2, err := supervisor.Adopt(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess2.Close() })
	guest2 := sess2.Guest()
	if guest2 == nil {
		t.Fatal("re-adopt returned no guest fd")
	}
	ino2, err := fileIno(guest2)
	if err != nil {
		t.Fatal(err)
	}
	if ino2 == ino1 {
		t.Fatal("re-adopt reused the stale guest fd")
	}
	dials := br.Dials()
	if len(dials) != 2 || dials[0] == dials[1] || dials[0] != ino1 || dials[1] != ino2 {
		t.Fatalf("dials %v fds %d %d: re-adopt must be a new Connect", dials, ino1, ino2)
	}
	if !sess2.Held() || sess2.State() != supervisor.SlotRunning || sess2.Status().State != types.VMRunning {
		t.Fatalf("readopt state %s held %v status %+v", sess2.State(), sess2.Held(), sess2.Status())
	}
	if err := writeByte(kept); err != nil {
		t.Fatalf("previous guest connection did not stay open: %v", err)
	}
	if err := writeByte(guest2); err != nil {
		t.Fatalf("fresh guest connection: %v", err)
	}
}

func testSupervisorKilled(t *testing.T, factory Factory) {
	if factory == nil {
		t.Fatal("nil factory")
	}
	leaks := goleak.IgnoreCurrent()
	prepare(t)
	t.Cleanup(func() { goleak.VerifyNone(t, leaks) })

	dir := shortRunDir(t)
	child := startSupervisorChild(t, dir)
	t.Cleanup(child.stop)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	insp := bootVM(t, ctx, dir)
	lease := leaseFrom(t, insp)
	if err := supervisor.WriteLease(dir, 0, lease); err != nil {
		t.Fatal(err)
	}
	sess, err := supervisor.Adopt(ctx, adoptReq(dir, lease, insp, "agent-live"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sess.Close() })
	if !sess.Held() || sess.Status().State != types.VMRunning {
		t.Fatalf("before kill state %s status %+v", sess.State(), sess.Status())
	}
	guest := sess.Guest()
	if guest == nil {
		t.Fatal("no guest fd")
	}
	assertSockType(t, guest, unix.SOCK_STREAM)

	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	wait, waitCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer waitCancel()
	started := time.Now()
	if err := sess.WaitLost(wait); err != nil {
		t.Fatalf("supervisor death was not visible after %s: %v (state %s)", time.Since(started), err, sess.State())
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("supervisor death took %s", elapsed)
	}
	if sess.State() != supervisor.SlotLost || sess.Held() {
		t.Fatalf("slot marked %s held %v", sess.State(), sess.Held())
	}
	if sess.Status().State != types.VMFailed {
		t.Fatalf("status after kill %+v", sess.Status())
	}
	assertGuestDead(t, guest)
}

func bootVM(t *testing.T, ctx context.Context, dir string) supervisor.Inspect {
	t.Helper()
	c, err := supervisor.Dial(ctx, supervisor.SocketPath(dir, 0), "agent-boot", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	insp, err := c.StartVM(ctx, supervisor.StartRequest{
		Spec: types.VMSpec{
			ID:          types.MachineID{Namespace: "ns", Name: "pod", UID: "uid-restart"},
			ImageRef:    "sha256:restart",
			CPU:         2,
			MemoryBytes: 1 << 30,
			AgentToken:  "tok",
		},
		BootID:      "boot-restart",
		HMACToken:   "token-restart",
		ImageDigest: "sha256:restart",
	})
	if err != nil {
		if strings.Contains(err.Error(), "hardware-pending") {
			t.Skip(err.Error())
		}
		t.Fatal(err)
	}
	if insp.State != string(types.VMRunning) || insp.BootID != "boot-restart" {
		t.Fatalf("start inspect %+v", insp)
	}
	return insp
}

func adoptReq(dir string, lease supervisor.Lease, insp supervisor.Inspect, version string) supervisor.AdoptRequest {
	return supervisor.AdoptRequest{
		Socket:        supervisor.SocketPath(dir, 0),
		BinaryVersion: version,
		Slot:          0,
		Lease:         lease,
		Pod: supervisor.PodPresence{
			Namespace: insp.Namespace,
			Name:      insp.PodName,
			UID:       insp.PodUID,
			Found:     true,
		},
	}
}

func leaseFrom(t *testing.T, insp supervisor.Inspect) supervisor.Lease {
	t.Helper()
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
		t.Fatal(err)
	}
	return lease
}

func shortRunDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "dn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func startInProcSupervisor(t *testing.T, dir string, rt runtime.Runtime) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := &supervisor.Server{
		Slot:          0,
		RunDir:        dir,
		RT:            rt,
		BinaryVersion: "sv-restart",
		Deadman:       time.Hour,
	}
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = srv.Serve(ctx)
		close(done)
	}()
	waitListen(t, supervisor.SocketPath(dir, 0), done, func() error { return serveErr })
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			select {
			case <-done:
				if serveErr != nil {
					t.Errorf("supervisor: %v", serveErr)
				}
			case <-time.After(5 * time.Second):
				t.Errorf("supervisor did not exit")
			}
		})
	}
}

func waitListen(t *testing.T, path string, done <-chan struct{}, errFn func() error) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("supervisor exited before %s: %v", path, errFn())
		default:
		}
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %s did not appear", path)
}

type childProc struct {
	cmd    *exec.Cmd
	exited chan struct{}
	err    error
	once   sync.Once
}

func startSupervisorChild(t *testing.T, dir string) *childProc {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		envSupervisorChild+"=1",
		envSupervisorRunDir+"="+dir,
		envSupervisorSlot+"=0",
	)
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &childProc{cmd: cmd, exited: make(chan struct{})}
	go func() {
		c.err = cmd.Wait()
		close(c.exited)
	}()
	deadline := time.Now().Add(8 * time.Second)
	path := supervisor.SocketPath(dir, 0)
	for time.Now().Before(deadline) {
		select {
		case <-c.exited:
			msg := strings.TrimSpace(stderr.String())
			if strings.Contains(msg, "hardware-pending") {
				t.Skip(msg)
			}
			t.Fatalf("supervisor child exited: %v: %s", c.err, msg)
		default:
		}
		if _, err := os.Stat(path); err == nil {
			return c
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.stop()
	t.Fatalf("socket %s did not appear: %s", path, stderr.String())
	return nil
}

func (c *childProc) stop() {
	if c == nil {
		return
	}
	c.once.Do(func() {
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		if c.exited != nil {
			select {
			case <-c.exited:
			case <-time.After(5 * time.Second):
			}
		}
	})
}

func assertSockType(t *testing.T, f *os.File, want int) {
	t.Helper()
	got, err := sockOptInt(f, unix.SO_TYPE)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("socket type %d, want %d", got, want)
	}
}

func assertGuestDead(t *testing.T, f *os.File) {
	t.Helper()
	started := time.Now()
	if err := f.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err := f.Read(make([]byte, 8))
	if err == nil || isDeadline(err) || (!errors.Is(err, io.EOF) && !errors.Is(err, syscall.ECONNRESET) && !errors.Is(err, syscall.EPIPE)) {
		t.Fatalf("guest read after supervisor death: %v", err)
	}
	if err := f.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	_, werr := f.Write([]byte{1})
	if werr == nil || isDeadline(werr) || (!errors.Is(werr, syscall.EPIPE) && !errors.Is(werr, syscall.ECONNRESET)) {
		t.Fatalf("guest write after supervisor death: %v", werr)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("guest fd reported death after %s", time.Since(started))
	}
}

func isDeadline(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded)
}

func writeByte(f *os.File) error {
	if err := f.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	_, err := f.Write([]byte{1})
	return err
}

func fileIno(f *os.File) (uint64, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var st unix.Stat_t
	var serr error
	if err := raw.Control(func(fd uintptr) {
		serr = unix.Fstat(int(fd), &st)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return uint64(st.Ino), nil
}

func sockOptInt(f *os.File, opt int) (int, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var got int
	var serr error
	if err := raw.Control(func(fd uintptr) {
		got, serr = unix.GetsockoptInt(int(fd), unix.SOL_SOCKET, opt)
	}); err != nil {
		return 0, err
	}
	return got, serr
}

func dupOSFile(f *os.File) (*os.File, error) {
	if f == nil {
		return nil, errors.New("nil file")
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
		return nil, errors.New("dup file")
	}
	return out, nil
}
