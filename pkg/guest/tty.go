// PTY-backed TTY execution for the guest agent. Contained here so the
// creack/pty dependency touches nothing else.
package guest

import (
	"context"
	"io"
	"os/exec"
	"sync"

	"github.com/creack/pty"
)

// runExecTTY runs cmd under a pseudo-terminal. Stdin frames flow into the
// PTY master (so ^C and other control bytes hit the guest line discipline),
// master output emits as stdout events, and TtyResize frames resize the
// terminal while the command runs. Every helper goroutine is tied to
// command completion or ctx cancellation: none outlives the session.
func (h *Handler) runExecTTY(ctx context.Context, cmd *exec.Cmd, stdin io.Reader, stdout io.Writer) (int, error) {
	resizeCh := make(chan TtyResize, 4)
	var sr *execStdinReader
	if s, ok := stdin.(*execStdinReader); ok {
		sr = s
		sr.resize = resizeCh
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return 1, err
	}

	var helpers sync.WaitGroup

	copyDone := make(chan struct{})
	helpers.Add(1)
	go func() {
		defer helpers.Done()
		defer close(copyDone)
		buf := make([]byte, 32<<10)
		for {
			n, rerr := ptmx.Read(buf)
			if n > 0 {
				if _, werr := stdout.Write(buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	stdinDone := make(chan struct{})
	helpers.Add(1)
	go func() {
		defer helpers.Done()
		defer close(stdinDone)
		if stdin == nil {
			return
		}
		buf := make([]byte, 32<<10)
		for {
			n, rerr := stdin.Read(buf)
			if n > 0 {
				if _, werr := ptmx.Write(buf[:n]); werr != nil {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	// resizeQuit is owned by this function alone: nothing else closes it,
	// and the goroutine never closes a channel it selects on.
	resizeQuit := make(chan struct{})
	helpers.Add(1)
	go func() {
		defer helpers.Done()
		for {
			select {
			case <-resizeQuit:
				return
			case <-ctx.Done():
				return
			case ev := <-resizeCh:
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(ev.Cols), Rows: uint16(ev.Rows)})
			}
		}
	}()

	err = cmd.Wait()
	// Unblock any reader parked on stdin frames now that the process is gone.
	if sr != nil {
		sr.abortNow()
	}
	// Closing the master unblocks the output copier even if the slave side
	// stays open. The deferred Close is a harmless second attempt.
	_ = ptmx.Close()
	close(resizeQuit)
	helpers.Wait()
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return 1, err
}
