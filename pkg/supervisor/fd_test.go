//go:build linux || darwin

package supervisor

import (
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRecvFDsAreCloseOnExec(t *testing.T) {
	before := countFDs(t)
	func() {
		a, b := unixPair(t)
		defer func() { _ = a.Close() }()
		defer func() { _ = b.Close() }()
		p := makePipe(t)
		defer func() { _ = p.w.Close() }()
		defer func() { _ = p.r.Close() }()
		if err := sendFDs(b, []*os.File{p.r}); err != nil {
			t.Fatal(err)
		}
		files, err := recvFDs(a, 1)
		if err != nil {
			t.Fatal(err)
		}
		defer closeFiles(files)
		if !fdCloexec(t, int(files[0].Fd())) {
			t.Fatal("received fd is not close-on-exec")
		}
	}()
	if after := countFDs(t); after != before {
		t.Fatalf("fd leak before %d after %d", before, after)
	}
}

func TestSendFDsSetsCloseOnExec(t *testing.T) {
	before := countFDs(t)
	func() {
		a, b := unixPair(t)
		defer func() { _ = a.Close() }()
		defer func() { _ = b.Close() }()
		p := makePipe(t)
		defer func() { _ = p.w.Close() }()
		defer func() { _ = p.r.Close() }()
		if _, err := unix.FcntlInt(p.r.Fd(), unix.F_SETFD, 0); err != nil {
			t.Fatal(err)
		}
		if fdCloexec(t, int(p.r.Fd())) {
			t.Fatal("fixture fd was already close-on-exec")
		}
		if err := sendFDs(b, []*os.File{p.r}); err != nil {
			t.Fatal(err)
		}
		files, err := recvFDs(a, 1)
		if err != nil {
			t.Fatal(err)
		}
		closeFiles(files)
		if !fdCloexec(t, int(p.r.Fd())) {
			t.Fatal("supervisor fd was sent without close-on-exec")
		}
	}()
	if after := countFDs(t); after != before {
		t.Fatalf("fd leak before %d after %d", before, after)
	}
}

func TestTruncatedRightsAreClosed(t *testing.T) {
	before := countFDs(t)
	func() {
		a, b := unixPair(t)
		defer func() { _ = a.Close() }()
		defer func() { _ = b.Close() }()
		p1 := makePipe(t)
		p2 := makePipe(t)
		defer func() { _ = p1.r.Close() }()
		defer func() { _ = p1.w.Close() }()
		defer func() { _ = p2.r.Close() }()
		defer func() { _ = p2.w.Close() }()
		rights := unix.UnixRights(int(p1.r.Fd()), int(p2.r.Fd()))
		if len(rights) < 2 {
			t.Fatalf("rights length %d", len(rights))
		}
		if _, _, err := b.WriteMsgUnix([]byte{1}, rights, nil); err != nil {
			t.Fatal(err)
		}
		files, err := readFDs(a, 2, make([]byte, len(rights)-1))
		closeFiles(files)
		if err == nil || !strings.Contains(err.Error(), "truncated") {
			t.Fatalf("err %v, want truncated control message", err)
		}
	}()
	if after := countFDs(t); after != before {
		t.Fatalf("fd leak before %d after %d", before, after)
	}
}

type pipePair struct {
	r *os.File
	w *os.File
}

func makePipe(t *testing.T) pipePair {
	t.Helper()
	var fds [2]int
	if err := unix.Pipe(fds[:]); err != nil {
		t.Fatal(err)
	}
	r := os.NewFile(uintptr(fds[0]), "r")
	w := os.NewFile(uintptr(fds[1]), "w")
	if r == nil || w == nil {
		if r != nil {
			_ = r.Close()
		}
		if w != nil {
			_ = w.Close()
		}
		t.Fatal("NewFile")
	}
	return pipePair{r: r, w: w}
}

func unixPair(t *testing.T) (*net.UnixConn, *net.UnixConn) {
	t.Helper()
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := os.NewFile(uintptr(fds[0]), "a")
	b := os.NewFile(uintptr(fds[1]), "b")
	ca, err := net.FileConn(a)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := net.FileConn(b)
	if err != nil {
		t.Fatal(err)
	}
	_ = a.Close()
	_ = b.Close()
	return ca.(*net.UnixConn), cb.(*net.UnixConn)
}

func fdCloexec(t *testing.T, fd int) bool {
	t.Helper()
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	return flags&unix.FD_CLOEXEC != 0
}

func countFDs(t *testing.T) int {
	t.Helper()
	path := "/proc/self/fd"
	if runtime.GOOS == "darwin" {
		path = "/dev/fd"
	}
	// Same finalizer race as fdCount. Drain, then require two reads to agree.
	runtime.GC()
	runtime.GC()
	prev := -1
	for i := 0; i < 20; i++ {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		names, err := f.Readdirnames(-1)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(names) == prev {
			return prev
		}
		prev = len(names)
		time.Sleep(5 * time.Millisecond)
		runtime.GC()
	}
	return prev
}
