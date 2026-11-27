package supervisor

import (
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"

	"golang.org/x/sys/unix"
)

// sendFDs writes one carrier byte plus SCM_RIGHTS. The caller still owns
// files and closes them. A zero-length payload does not carry rights on
// every kernel, so the carrier byte is required.
func sendFDs(conn *net.UnixConn, files []*os.File) error {
	fds := make([]int, len(files))
	for i, f := range files {
		if f == nil {
			return fmt.Errorf("nil file descriptor")
		}
		fd := int(f.Fd())
		unix.CloseOnExec(fd)
		fds[i] = fd
	}
	oob := unix.UnixRights(fds...)
	n, _, err := conn.WriteMsgUnix([]byte{1}, oob, nil)
	runtime.KeepAlive(files)
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("short fd carrier write: %d", n)
	}
	return nil
}

// recvFDs reads the carrier byte and returns exactly want descriptors.
// The caller owns the files. A short or truncated control message closes
// whatever arrived and returns an error.
func recvFDs(conn *net.UnixConn, want int) ([]*os.File, error) {
	return readFDs(conn, want, make([]byte, unix.CmsgSpace(want*8+64)))
}

// readFDs is recvFDs with an explicit control buffer. Tests pass a short
// buffer to force MSG_CTRUNC.
func readFDs(conn *net.UnixConn, want int, oob []byte) ([]*os.File, error) {
	if want < 1 {
		return nil, fmt.Errorf("fd count %d", want)
	}
	if len(oob) == 0 {
		oob = make([]byte, 1)
	}
	buf := make([]byte, 8)
	// Darwin can install every SCM_RIGHTS descriptor and still set
	// MSG_CTRUNC without putting those numbers in the truncated buffer.
	// The snapshot is how those descriptors get closed.
	before, snapErr := listFDs()
	n, oobn, flags, _, err := conn.ReadMsgUnix(buf, oob)
	// Parse before deciding the call failed. A truncated control message
	// can still install descriptors, and those must be closed.
	fds, perr := rightsFrom(oob[:oobn])
	fail := func(e error, sweep bool) ([]*os.File, error) {
		closeRaw(fds)
		if sweep && snapErr == nil {
			closeArrivals(before)
		}
		return nil, e
	}
	if err != nil {
		return fail(err, false)
	}
	if flags&unix.MSG_CTRUNC != 0 {
		return fail(fmt.Errorf("fd control message truncated"), true)
	}
	if perr != nil {
		return fail(perr, true)
	}
	if n < 1 {
		return fail(io.ErrUnexpectedEOF, false)
	}
	if len(fds) != want {
		return fail(fmt.Errorf("received %d file descriptors, want %d", len(fds), want), false)
	}
	files := make([]*os.File, len(fds))
	for i, fd := range fds {
		// NewFile only attaches the poller when the descriptor is
		// already non-blocking. The agent reads these files.
		if e := unix.SetNonblock(fd, true); e != nil {
			closeRaw(fds[i:])
			closeFiles(files[:i])
			return nil, e
		}
		f := os.NewFile(uintptr(fd), "supervisor-fd")
		if f == nil {
			_ = unix.Close(fd)
			closeRaw(fds[i+1:])
			closeFiles(files[:i])
			return nil, fmt.Errorf("supervisor fd")
		}
		files[i] = f
	}
	return files, nil
}

// rightsFrom parses SCM_RIGHTS and marks every received descriptor
// close-on-exec before anything else can see it. Go's Linux recv path
// also sets the flag. Darwin does not, so this call is required there.
func rightsFrom(oob []byte) ([]int, error) {
	if len(oob) == 0 {
		return nil, nil
	}
	scms, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var fds []int
	for _, scm := range scms {
		got, perr := unix.ParseUnixRights(&scm)
		if perr != nil {
			continue
		}
		for _, fd := range got {
			unix.CloseOnExec(fd)
			fds = append(fds, fd)
		}
	}
	return fds, nil
}

func closeRaw(fds []int) {
	for _, fd := range fds {
		if fd >= 0 {
			unix.CloseOnExec(fd)
			_ = unix.Close(fd)
		}
	}
}

// listFDs is the process's open descriptors. The directory used to read
// them is closed before the result is returned. Darwin's fdopendir dups
// that directory, and recvmsg will reuse the dup's number for a
// descriptor that just arrived. Numbers that are already closed are
// left out so the sweep does not treat a reused number as old.
func listFDs() (map[int]struct{}, error) {
	path := "/proc/self/fd"
	if runtime.GOOS == "darwin" {
		path = "/dev/fd"
	}
	dir, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	names, rdErr := dir.Readdirnames(-1)
	closeErr := dir.Close()
	if rdErr != nil {
		return nil, rdErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	out := make(map[int]struct{}, len(names))
	for _, name := range names {
		n, conv := strconv.Atoi(name)
		if conv != nil || !fdLive(n) {
			continue
		}
		out[n] = struct{}{}
	}
	return out, nil
}

func fdLive(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

// closeArrivals closes descriptors that appeared after before. before
// must be a successful listFDs result. A nil map means the snapshot
// failed, and closing "new" descriptors would close the whole process.
func closeArrivals(before map[int]struct{}) {
	if before == nil {
		return
	}
	after, err := listFDs()
	if err != nil {
		return
	}
	for fd := range after {
		if _, ok := before[fd]; ok {
			continue
		}
		unix.CloseOnExec(fd)
		_ = unix.Close(fd)
	}
}

func setCloexec(f *os.File) {
	if f == nil {
		return
	}
	unix.CloseOnExec(int(f.Fd()))
	runtime.KeepAlive(f)
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}
