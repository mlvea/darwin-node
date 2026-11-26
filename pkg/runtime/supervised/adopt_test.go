package supervised

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/simvz"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"
	"go.uber.org/goleak"
	"golang.org/x/sys/unix"
)

const (
	envSupervisedChild  = "DARWIN_NODE_SUPERVISED_CHILD"
	envSupervisedRunDir = "DARWIN_NODE_SUPERVISED_RUN_DIR"
	envSupervisedSlot   = "DARWIN_NODE_SUPERVISED_SLOT"
)

// TestNodeAgentRestartReadoptsFreshConnect is the runtime path for a
// restarted node-agent. The live supervisor keeps the VM. The new agent
// adopts it with supervisor.Adopt: a new Connect, not a saved fd, and
// not a second boot.
func TestNodeAgentRestartReadoptsFreshConnect(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	before, err := simvz.FDCount()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		goleak.VerifyNone(t, leaks)
		got, ferr := simvz.FDCount()
		if ferr != nil {
			t.Error(ferr)
			return
		}
		if got != before {
			t.Errorf("fd leak: before %d after %d", before, got)
		}
	})

	dir := socketRunDir(t)
	sim := simvz.New(simvz.Options{Dir: dir})
	bridge := &connectCount{inner: sim}
	stop := startCountedSupervisor(t, dir, bridge)
	t.Cleanup(stop)

	ctx := context.Background()
	first := New(Options{
		RunDir:        dir,
		Slots:         1,
		Deadman:       time.Hour,
		VMRuntime:     simvz.Name,
		BinaryVersion: "agent-old",
	})
	m, err := first.Create(ctx, podSpec())
	if err != nil {
		t.Fatal(err)
	}
	var second runtime.Machine
	t.Cleanup(func() {
		if second != nil {
			_ = second.Stop(context.Background(), 0)
		}
		_ = m.Stop(context.Background(), 0)
	})
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if m.Status().State != types.VMRunning {
		t.Fatalf("status %+v", m.Status())
	}
	dials := bridge.dials()
	if len(dials) != 1 {
		t.Fatalf("first attach dials %v, want one fresh Connect from Adopt", dials)
	}
	ino1 := guestIno(t, m)
	if ino1 != dials[0] {
		t.Fatalf("adopted guest inode %d, Connect recorded %d", ino1, dials[0])
	}
	kept, err := dupFile(m.(*machine).sess.Guest())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = kept.Close() })

	m.(*machine).dropConn()

	next := New(Options{
		RunDir:        dir,
		Slots:         1,
		Deadman:       time.Hour,
		VMRuntime:     simvz.Name,
		BinaryVersion: "agent-new",
	})
	got, err := next.Recover(ctx, []supervisor.PodPresence{{
		Namespace: "ns", Name: "pod", UID: "uid-1", Found: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("recovered %d machines", len(got))
	}
	second = got[0]
	if sim.Running() != 1 || bridge.starts() != 1 {
		t.Fatalf("running %d starts %d: re-adopt booted a new VM", sim.Running(), bridge.starts())
	}
	if second.Status().State != types.VMRunning {
		t.Fatalf("readopt status %+v", second.Status())
	}
	dials = bridge.dials()
	if len(dials) != 2 || dials[0] == dials[1] {
		t.Fatalf("dials %v: re-adopt must be a second Connect", dials)
	}
	ino2 := guestIno(t, second)
	if ino2 == ino1 || ino2 != dials[1] {
		t.Fatalf("re-adopt guest inode %d, first %d, Connects %v", ino2, ino1, dials)
	}
	if err := writeByte(kept); err != nil {
		t.Fatalf("previous guest connection did not stay open: %v", err)
	}
	if err := writeByte(second.(*machine).sess.Guest()); err != nil {
		t.Fatalf("fresh guest connection: %v", err)
	}
}

// TestSupervisorKillFailsSlotOnce is a SIGKILL of the supervisor process.
// The living agent marks the machine failed ("supervisor lost") within
// 2s and releases the slot one time. A later Stop must not free a slot
// a new VM has taken.
func TestSupervisorKillFailsSlotOnce(t *testing.T) {
	leaks := goleak.IgnoreCurrent()
	before, err := simvz.FDCount()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		goleak.VerifyNone(t, leaks)
		got, ferr := simvz.FDCount()
		if ferr != nil {
			t.Error(ferr)
			return
		}
		if got != before {
			t.Errorf("fd leak: before %d after %d", before, got)
		}
	})

	dir := socketRunDir(t)
	child := startSupervisorChild(t, dir)
	t.Cleanup(child.stop)

	ctx := context.Background()
	rt := New(Options{
		RunDir:        dir,
		Slots:         1,
		Deadman:       time.Hour,
		VMRuntime:     simvz.Name,
		BinaryVersion: "agent-live",
	})
	m, err := rt.Create(ctx, podSpec())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background(), 0) })
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if m.Status().State != types.VMRunning {
		t.Fatalf("before kill %+v", m.Status())
	}
	if !slotHeld(rt, 0) {
		t.Fatal("slot was not held before the supervisor died")
	}

	if err := child.cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	deadline := started.Add(2 * time.Second)
	var st types.VMStatus
	for {
		st = m.Status()
		if st.State == types.VMFailed && st.Message == "supervisor lost" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status %+v after %s, want Failed %q within 2s", st, time.Since(started), "supervisor lost")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("supervisor death took %s", time.Since(started))
	}
	if slotHeld(rt, 0) {
		t.Fatal("failed slot was not released")
	}
	rt.mark(0)
	_ = m.Stop(ctx, 0)
	if !slotHeld(rt, 0) {
		t.Fatal("Stop released the slot a second time")
	}
}

func runSupervisedChild() error {
	dir := os.Getenv(envSupervisedRunDir)
	if dir == "" {
		return errors.New("supervisor run dir is empty")
	}
	slot := 0
	if v := os.Getenv(envSupervisedSlot); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("supervisor slot: %w", err)
		}
		slot = n
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	return (&supervisor.Server{
		Slot:          slot,
		RunDir:        dir,
		RT:            simvz.New(simvz.Options{Dir: dir}),
		BinaryVersion: "sv-child",
		Deadman:       time.Hour,
	}).Serve(ctx)
}

func guestIno(t *testing.T, m runtime.Machine) uint64 {
	t.Helper()
	mm, ok := m.(*machine)
	if !ok || mm == nil || mm.sess == nil || mm.sess.Guest() == nil {
		t.Fatal("machine has no adopted guest fd")
	}
	ino, err := fileIno(mm.sess.Guest())
	if err != nil {
		t.Fatal(err)
	}
	return ino
}

func slotHeld(r *Runtime, slot int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slot >= 0 && slot < len(r.used) && r.used[slot]
}

type connectCount struct {
	inner  *simvz.Runtime
	mu     sync.Mutex
	nStart int
	inos   []uint64
}

func (c *connectCount) dials() []uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]uint64, len(c.inos))
	copy(out, c.inos)
	return out
}

func (c *connectCount) starts() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nStart
}

func (c *connectCount) Name() types.RuntimeName { return c.inner.Name() }

func (c *connectCount) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := c.inner.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	src, ok := m.(runtime.ChannelSource)
	if !ok {
		return nil, errors.New("simvz machine has no channels")
	}
	var closer runtime.ChannelCloser
	if cl, ok := m.(runtime.ChannelCloser); ok {
		closer = cl
	}
	return &connectVM{Machine: m, src: src, closer: closer, rt: c}, nil
}

type connectVM struct {
	runtime.Machine
	src    runtime.ChannelSource
	closer runtime.ChannelCloser
	rt     *connectCount
}

func (m *connectVM) Start(ctx context.Context) error {
	if err := m.Machine.Start(ctx); err != nil {
		return err
	}
	m.rt.mu.Lock()
	m.rt.nStart++
	m.rt.mu.Unlock()
	return nil
}

func (m *connectVM) OpenChannels(ctx context.Context) (*os.File, *os.File, error) {
	netFD, agentFD, err := m.src.OpenChannels(ctx)
	if err != nil {
		return nil, nil, err
	}
	ino, err := fileIno(agentFD)
	if err != nil {
		_ = netFD.Close()
		_ = agentFD.Close()
		return nil, nil, err
	}
	m.rt.mu.Lock()
	m.rt.inos = append(m.rt.inos, ino)
	m.rt.mu.Unlock()
	return netFD, agentFD, nil
}

func (m *connectVM) CloseChannels() error {
	if m.closer == nil {
		return nil
	}
	return m.closer.CloseChannels()
}

func startCountedSupervisor(t *testing.T, dir string, rt runtime.Runtime) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	srv := &supervisor.Server{
		Slot:          0,
		RunDir:        dir,
		RT:            rt,
		BinaryVersion: "sv-sim",
		Deadman:       time.Hour,
	}
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = srv.Serve(ctx)
		close(done)
	}()
	waitSocket(t, supervisor.SocketPath(dir, 0), done, &serveErr)
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

type childProc struct {
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	exited chan struct{}
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
		envSupervisedChild+"=1",
		envSupervisedRunDir+"="+dir,
		envSupervisedSlot+"=0",
	)
	var stderr bytes.Buffer
	cmd.Stdout = &stderr
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &childProc{cmd: cmd, stderr: &stderr, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(c.exited)
	}()
	waitChildSocket(t, c, supervisor.SocketPath(dir, 0))
	return c
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

func waitSocket(t *testing.T, path string, done <-chan struct{}, errp *error) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-done:
			t.Fatalf("supervisor exited before %s: %v", path, *errp)
		default:
		}
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("socket %s did not appear", path)
}

func waitChildSocket(t *testing.T, c *childProc, path string) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-c.exited:
			t.Fatalf("supervisor child exited before %s: %s", path, c.stderr.String())
		default:
		}
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.stop()
	t.Fatalf("socket %s did not appear: %s", path, c.stderr.String())
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

func dupFile(f *os.File) (*os.File, error) {
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

func writeByte(f *os.File) error {
	if err := f.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	_, err := f.Write([]byte{1})
	return err
}
