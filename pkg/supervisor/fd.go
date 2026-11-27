package supervisor

import (
	"fmt"
	"io"
	"net"
	"os"
	"runtime"

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
	n, oobn, flags, _, err := conn.ReadMsgUnix(buf, oob)
	// Parse before deciding the call failed. A truncated control message
	// can still install descriptors, and those must be closed.
	fds, perr := rightsFrom(oob[:oobn])
	fail := func(e error) ([]*os.File, error) {
		closeRaw(fds)
		return nil, e
	}
	if err != nil {
		return fail(err)
	}
	if flags&unix.MSG_CTRUNC != 0 {
		return fail(fmt.Errorf("fd control message truncated"))
	}
	if perr != nil {
		return fail(perr)
	}
	if n < 1 {
		return fail(io.ErrUnexpectedEOF)
	}
	if len(fds) != want {
		return fail(fmt.Errorf("received %d file descriptors, want %d", len(fds), want))
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
			_ = unix.Close(fd)
		}
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
