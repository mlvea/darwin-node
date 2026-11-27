package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/types"
	"golang.org/x/sys/unix"
)

// Server is one slot. It listens on <run dir>/slot-N.sock, owns at most
// one VM, and returns from Serve after that VM stops or ctx is cancelled.
// A clean stop returns nil so the process can exit 0.
//
// Config fields are read once, at the start of Serve. Changing them
// afterwards has no effect.
type Server struct {
	Slot           int
	RunDir         string
	RT             runtime.Runtime
	BinaryVersion  string
	Deadman        time.Duration
	AllowUID       int
	AllowUIDSet    bool
	AdvertiseMajor int
	AdvertiseMinor int
	// StopDeadline caps a hung VM stop after the caller's grace period.
	// Zero means 10s. Tests set a shorter cap.
	StopDeadline time.Duration
	// Exit replaces os.Exit when that cap is exceeded. Nil means os.Exit.
	Exit func(int)

	mu      sync.Mutex
	conns   int
	vm      runtime.Machine
	bootID  string
	token   string
	digest  string
	ns      string
	podName string
	podUID  string
	paused  bool
	active  map[net.Conn]struct{}

	vmOp sync.Mutex

	deadMu    sync.Mutex
	deadArmed bool
	deadAt    time.Time
	lastBeat  time.Time

	ctx      context.Context
	cancel   context.CancelFunc
	exit     chan struct{}
	exitOnce sync.Once
	wg       sync.WaitGroup

	ln         net.Listener
	sock       string
	acceptDone chan struct{}

	uid     int
	major   int
	minor   int
	version string
	deadman time.Duration
	started time.Time
}

// Serve listens until the VM stops or ctx is done, then stops the VM and
// returns. A cancelled context after a clean stop still returns nil.
func (s *Server) Serve(ctx context.Context) error {
	if err := s.prepare(ctx); err != nil {
		return err
	}
	s.wg.Add(1)
	go s.deadLoop()

	go func() {
		s.acceptLoop()
		close(s.acceptDone)
	}()

	select {
	case <-s.ctx.Done():
	case <-s.exit:
	}
	return s.shutdown()
}

func (s *Server) prepare(ctx context.Context) error {
	if err := RaiseNOFILE(); err != nil {
		slog.Warn("supervisor nofile limit", "err", err)
	}
	if s.RT == nil {
		return fmt.Errorf("supervisor runtime is nil")
	}
	if s.Slot < 0 || s.Slot >= types.AppleMaxConcurrentVMs {
		return fmt.Errorf("supervisor slot must be 0 or 1, got %d", s.Slot)
	}
	s.sock = SocketPath(s.RunDir, s.Slot)
	if err := checkSocketPath(s.sock); err != nil {
		return err
	}
	if err := prepareRunDir(s.RunDir); err != nil {
		return err
	}
	s.uid = os.Getuid()
	if s.AllowUIDSet {
		s.uid = s.AllowUID
	}
	s.major = ProtocolMajor
	if s.AdvertiseMajor != 0 {
		s.major = s.AdvertiseMajor
	}
	s.minor = ProtocolMinor
	if s.AdvertiseMinor != 0 {
		s.minor = s.AdvertiseMinor
	}
	s.version = s.BinaryVersion
	if s.version == "" {
		s.version = "dev"
	}
	s.deadman = s.Deadman
	if s.deadman <= 0 {
		s.deadman = 5 * time.Minute
	}
	s.started = time.Now()
	s.exit = make(chan struct{})
	s.acceptDone = make(chan struct{})
	s.active = map[net.Conn]struct{}{}
	s.ctx, s.cancel = context.WithCancel(ctx)

	if err := os.Remove(s.sock); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.cancel()
		return err
	}
	ln, err := net.Listen("unix", s.sock)
	if err != nil {
		s.cancel()
		return err
	}
	if err := os.Chmod(s.sock, 0o600); err != nil {
		_ = ln.Close()
		s.cancel()
		return err
	}
	s.ln = ln
	slog.Info("supervisor listening", "slot", s.Slot, "socket", s.sock, "protocol", s.major, "binary", s.version)
	return nil
}

func (s *Server) shutdown() error {
	if s.cancel != nil {
		s.cancel()
	}
	if s.ln != nil {
		_ = s.ln.Close()
	}
	<-s.acceptDone
	s.closeActive()
	s.vmOp.Lock()
	_, stopErr := s.stopVM(context.Background(), 5*time.Second)
	s.vmOp.Unlock()
	s.wg.Wait()
	if s.sock != "" {
		_ = os.Remove(s.sock)
	}
	return stopErr
}

func (s *Server) signalExit() {
	s.exitOnce.Do(func() { close(s.exit) })
}

func (s *Server) acceptLoop() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.track(c)
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.untrack(c)
			s.serveConn(c)
		}()
	}
}

func (s *Server) track(c net.Conn) {
	s.mu.Lock()
	s.active[c] = struct{}{}
	s.mu.Unlock()
}

func (s *Server) untrack(c net.Conn) {
	s.mu.Lock()
	delete(s.active, c)
	s.mu.Unlock()
}

func (s *Server) closeActive() {
	s.mu.Lock()
	conns := make([]net.Conn, 0, len(s.active))
	for c := range s.active {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *Server) serveConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return
	}
	uid, err := peerUID(uc)
	if err != nil {
		if errors.Is(err, errPeerGone) {
			return
		}
		_ = writeMsg(uc, errMsg(codePeer, err.Error()))
		return
	}
	if uid != s.uid {
		_ = writeMsg(uc, errMsg(codePeer, "peer uid mismatch"))
		return
	}
	if err := writeMsg(uc, s.hello()); err != nil {
		return
	}
	peer, err := readMsg(uc)
	if err != nil {
		return
	}
	if peer.Op != opHello || peer.ProtocolMajor != s.major {
		_ = writeMsg(uc, errMsg(codeMajor, "protocol major mismatch"))
		return
	}
	ready := message{Op: opReady, OK: true, ProtocolMajor: s.major, ProtocolMinor: s.minor, BinaryVersion: s.version, Slot: s.Slot}
	if err := writeMsg(uc, ready); err != nil {
		return
	}

	s.noteConn(1)
	defer s.noteConn(-1)

	for {
		if s.ctx.Err() != nil {
			return
		}
		req, err := readMsg(uc)
		if err != nil {
			return
		}
		s.touch()
		resp, fds, done, derr := s.dispatch(s.ctx, req)
		if derr != nil && resp.Err == "" {
			resp = errMsg("", derr.Error())
		}
		resp.Op = opResponse
		resp.ID = req.ID
		if werr := writeMsg(uc, resp); werr != nil {
			closeFiles(fds)
			return
		}
		if len(fds) > 0 {
			if werr := sendFDs(uc, fds); werr != nil {
				closeFiles(fds)
				return
			}
			closeFiles(fds)
		}
		if done {
			s.signalExit()
			return
		}
	}
}

func (s *Server) hello() message {
	return message{
		Op:            opHello,
		OK:            true,
		ProtocolMajor: s.major,
		ProtocolMinor: s.minor,
		BinaryVersion: s.version,
		Role:          "supervisor",
		Slot:          s.Slot,
		PID:           os.Getpid(),
		StartTimeNS:   s.started.UnixNano(),
	}
}

func (s *Server) noteConn(delta int) {
	s.mu.Lock()
	s.conns += delta
	n := s.conns
	running := s.vm != nil
	s.mu.Unlock()
	if n > 0 {
		s.setDead(false)
		return
	}
	if delta < 0 && running {
		s.setDead(true)
	}
}

func (s *Server) setDead(arm bool) {
	s.deadMu.Lock()
	s.deadArmed = arm
	if arm {
		s.deadAt = time.Now().Add(s.deadman)
	}
	s.deadMu.Unlock()
}

func (s *Server) touch() {
	s.deadMu.Lock()
	s.lastBeat = time.Now()
	if s.deadArmed {
		s.deadAt = time.Now().Add(s.deadman)
	}
	s.deadMu.Unlock()
}

func (s *Server) deadLoop() {
	defer s.wg.Done()
	step := s.deadman / 10
	if step < 5*time.Millisecond {
		step = 5 * time.Millisecond
	}
	if step > time.Second {
		step = time.Second
	}
	tick := time.NewTicker(step)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			s.deadMu.Lock()
			fire := s.deadArmed && !time.Now().Before(s.deadAt)
			if fire {
				s.deadArmed = false
			}
			s.deadMu.Unlock()
			if fire {
				slog.Info("supervisor deadman", "slot", s.Slot, "after", s.deadman)
				s.signalExit()
				return
			}
		}
	}
}

// maxSocketPathLen is the longest pathname bind accepts for an AF_UNIX
// socket. sun_path reserves one byte for the terminating NUL (103 on
// macOS, 107 on Linux).
func maxSocketPathLen() int {
	var raw unix.RawSockaddrUnix
	return len(raw.Path) - 1
}

func checkSocketPath(path string) error {
	max := maxSocketPathLen()
	if len(path) > max {
		return fmt.Errorf("supervisor socket path is %d bytes; this OS allows %d (%s)", len(path), max, path)
	}
	return nil
}

func prepareRunDir(dir string) error {
	if dir == "" {
		return fmt.Errorf("supervisor run dir is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("supervisor run dir %s is a symlink", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("supervisor run dir %s is not a directory", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	fi, err = os.Lstat(dir)
	if err != nil {
		return err
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("supervisor run dir %s is mode %#o, want 0700", dir, perm)
	}
	var st unix.Stat_t
	if err := unix.Stat(dir, &st); err != nil {
		return err
	}
	if int(st.Uid) != os.Getuid() {
		return fmt.Errorf("supervisor run dir %s is owned by uid %d, not %d", dir, st.Uid, os.Getuid())
	}
	return nil
}
