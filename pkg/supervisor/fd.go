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
		fds[i] = int(f.Fd())
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
	buf := make([]byte, 8)
	n, oobn, flags, _, err := conn.ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, err
	}
	if n < 1 {
		return nil, io.ErrUnexpectedEOF
	}
	if flags&unix.MSG_CTRUNC != 0 {
		return nil, fmt.Errorf("fd control message truncated")
	}
	scms, err := unix.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return nil, err
	}
	var files []*os.File
	for _, scm := range scms {
		got, perr := unix.ParseUnixRights(&scm)
		if perr != nil {
			continue
		}
		for _, fd := range got {
			unix.CloseOnExec(fd)
			// NewFile only attaches the poller when the descriptor is
			// already non-blocking. The agent reads these files.
			if e := unix.SetNonblock(fd, true); e != nil {
				_ = unix.Close(fd)
				closeFiles(files)
				return nil, e
			}
			files = append(files, os.NewFile(uintptr(fd), "supervisor-fd"))
		}
	}
	if len(files) != want {
		closeFiles(files)
		return nil, fmt.Errorf("received %d file descriptors, want %d", len(files), want)
	}
	return files, nil
}

func closeFiles(files []*os.File) {
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}
}
