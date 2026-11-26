package supervisor

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/mlvea/darwin-node/pkg/types"
	"golang.org/x/sys/unix"
)

// SlotState is what the node-agent believes about one supervisor slot.
type SlotState string

const (
	// SlotRunning means this session holds the slot and the supervisor
	// connection is still up.
	SlotRunning SlotState = "running"
	// SlotLost means the supervisor process or its guest fd went away
	// while this session was still holding the slot.
	SlotLost SlotState = "lost"
	// SlotClosed means the agent dropped the session on purpose.
	SlotClosed SlotState = "closed"
)

// AdoptRequest is a re-attach to a VM the supervisor already started.
// Lease and Pod are the fence. Adopt does not start a VM and does not
// accept a file descriptor from a previous session.
type AdoptRequest struct {
	Socket        string
	BinaryVersion string
	Slot          int
	Lease         Lease
	Pod           PodPresence
}

// Session is one node-agent hold on a slot: the control connection plus
// the guest fd from this adopt's AttachChannels.
type Session struct {
	mu     sync.Mutex
	client *Client
	slot   int
	guest  *os.File
	nic    *os.File
	status types.VMStatus
	state  SlotState
	closed bool

	lost chan struct{}
	done chan struct{}
	once sync.Once
	// doneOnce closes done on loss and on Close. Waiters must leave
	// either way, or a voluntary close leaks the wait goroutine.
	doneOnce sync.Once
	wg       sync.WaitGroup

	ctrlFD  int
	guestFD int
	wakeR   int
	wakeW   int
}

// Adopt dials the supervisor, checks the boot lease, and attaches a new
// guest connection. The returned guest fd is the one AttachChannels just
// received. It is never a descriptor Adopt was given.
func Adopt(ctx context.Context, req AdoptRequest) (*Session, error) {
	c, err := Dial(ctx, req.Socket, req.BinaryVersion, req.Slot)
	if err != nil {
		return nil, err
	}
	insp, err := c.InspectVM(ctx)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
	if err := Fence(req.Lease, insp, req.Pod); err != nil {
		_ = c.Close()
		return nil, err
	}
	nic, guest, err := c.AttachChannels(ctx)
	if err != nil {
		_ = c.Close()
		return nil, err
	}
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
	s := &Session{
		client:  c,
		slot:    c.Slot(),
		guest:   guest,
		nic:     nic,
		status:  st,
		state:   SlotRunning,
		lost:    make(chan struct{}),
		done:    make(chan struct{}),
		ctrlFD:  -1,
		guestFD: -1,
		wakeR:   -1,
		wakeW:   -1,
	}
	if err := s.startWatch(); err != nil {
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

// Slot is the supervisor slot this session adopted.
func (s *Session) Slot() int {
	if s == nil {
		return -1
	}
	return s.slot
}

// State is the slot mark. SlotLost replaces SlotRunning when the
// supervisor disappears. Close does not report a live slot.
func (s *Session) State() SlotState {
	if s == nil {
		return SlotClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

// Held reports whether this session still counts the slot as occupied by
// a living supervisor.
func (s *Session) Held() bool { return s.State() == SlotRunning }

// Status is the VM state from adopt. After the supervisor is lost it is
// Failed, so a cached Running value is not served for a dead process.
func (s *Session) Status() types.VMStatus {
	if s == nil {
		return types.VMStatus{State: types.VMFailed, Message: "supervisor lost"}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status
	if s.state == SlotLost {
		st.State = types.VMFailed
		st.Message = "supervisor lost"
		st.AgentOK = false
	}
	return st
}

// Client is the control connection from this adopt. Close owns it.
// The caller uses it for inspect, heartbeat, and stop, and must not
// close it.
func (s *Session) Client() *Client {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.client
}

// WaitDone blocks until the slot is lost or the session is closed.
// The state after the wait says which one happened.
func (s *Session) WaitDone(ctx context.Context) SlotState {
	if s == nil {
		return SlotClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-s.done:
	case <-ctx.Done():
	}
	return s.State()
}

// Guest is the connected guest fd from this adopt. The caller must not
// close it; Close does.
func (s *Session) Guest() *os.File {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.guest
}

// WaitLost blocks until the supervisor is marked lost or ctx ends.
func (s *Session) WaitLost(ctx context.Context) error {
	if s == nil {
		return context.Canceled
	}
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-s.lost:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Close drops the control connection and the fds from this adopt. A
// second call is a no-op. Closing is the agent leaving, not the
// supervisor dying, so a running slot becomes SlotClosed.
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	if s.state == SlotRunning {
		s.state = SlotClosed
	}
	s.mu.Unlock()
	s.signalDone()
	s.wake()
	s.wg.Wait()
	return s.closeFDs()
}

// errWatchLost is the watcher's report that a supervised connection
// closed. It is not a voluntary Close.
var errWatchLost = errors.New("supervisor watch lost")

// startWatch waits on the control connection and the guest fd. The wait
// is edge-triggered, so a guest payload that is still queued does not
// spin the watcher. A blocked wait wakes as soon as the kernel closes
// the other end, which is what a SIGKILL of the supervisor looks like
// to this process.
func (s *Session) startWatch() error {
	ctrl, err := dupConn(s.client.raw)
	if err != nil {
		return err
	}
	guest, err := dupRaw(s.guest)
	if err != nil {
		_ = unix.Close(ctrl)
		return err
	}
	var pipe [2]int
	if err := unix.Pipe(pipe[:]); err != nil {
		_ = unix.Close(ctrl)
		_ = unix.Close(guest)
		return err
	}
	r, w := pipe[0], pipe[1]
	unix.CloseOnExec(r)
	unix.CloseOnExec(w)
	if err := unix.SetNonblock(w, true); err != nil {
		_ = unix.Close(ctrl)
		_ = unix.Close(guest)
		_ = unix.Close(r)
		_ = unix.Close(w)
		return err
	}
	s.ctrlFD = ctrl
	s.guestFD = guest
	s.wakeR = r
	s.wakeW = w
	s.wg.Add(1)
	go s.watch(r, ctrl, guest)
	return nil
}

func (s *Session) watch(wakeR, ctrlFD, guestFD int) {
	defer s.wg.Done()
	if err := watchFDs(wakeR, ctrlFD, guestFD); err != nil {
		s.markLost()
	}
}

func (s *Session) markLost() {
	s.once.Do(func() {
		s.mu.Lock()
		if s.state == SlotRunning {
			s.state = SlotLost
		}
		s.mu.Unlock()
		close(s.lost)
		s.signalDone()
	})
}

func (s *Session) signalDone() {
	s.doneOnce.Do(func() {
		if s.done != nil {
			close(s.done)
		}
	})
}

func (s *Session) wake() {
	if s.wakeW < 0 {
		return
	}
	_, _ = unix.Write(s.wakeW, []byte{1})
}

func (s *Session) closeFDs() error {
	s.mu.Lock()
	guest := s.guest
	nic := s.nic
	client := s.client
	s.guest = nil
	s.nic = nil
	s.client = nil
	fds := []int{s.ctrlFD, s.guestFD, s.wakeR, s.wakeW}
	s.ctrlFD, s.guestFD, s.wakeR, s.wakeW = -1, -1, -1, -1
	s.mu.Unlock()

	var err error
	if guest != nil {
		err = guest.Close()
	}
	if nic != nil {
		_ = nic.Close()
	}
	if client != nil {
		if cerr := client.Close(); err == nil {
			err = cerr
		}
	}
	for _, fd := range fds {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
	}
	return err
}

func deadFD(fd int, rev int16) bool {
	if rev == 0 {
		return false
	}
	if rev&unix.POLLNVAL != 0 {
		return true
	}
	var buf [1]byte
	n, _, err := unix.Recvfrom(fd, buf[:], unix.MSG_PEEK|unix.MSG_DONTWAIT)
	if err == nil {
		return n == 0
	}
	if errors.Is(err, unix.EINTR) {
		return false
	}
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
		return rev&(unix.POLLHUP|unix.POLLERR) != 0
	}
	return true
}

func dupConn(c *net.UnixConn) (int, error) {
	if c == nil {
		return -1, errors.New("nil control connection")
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return -1, err
	}
	return dupRawConn(raw)
}

func dupRaw(f *os.File) (int, error) {
	if f == nil {
		return -1, errors.New("nil guest fd")
	}
	raw, err := f.SyscallConn()
	if err != nil {
		return -1, err
	}
	return dupRawConn(raw)
}

func dupRawConn(raw syscall.RawConn) (int, error) {
	var nfd int
	var derr error
	if err := raw.Control(func(fd uintptr) {
		nfd, derr = unix.Dup(int(fd))
	}); err != nil {
		return -1, err
	}
	if derr != nil {
		return -1, derr
	}
	unix.CloseOnExec(nfd)
	return nfd, nil
}
