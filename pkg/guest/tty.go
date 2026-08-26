// PTY-backed TTY execution for the guest agent. Contained here so the
// creack/pty dependency touches nothing else.
package guest

import (
	"context"
	"io"
	"os/exec"

	"github.com/creack/pty"
)

// runExecTTY runs cmd under a pseudo-terminal. Stdin frames flow into the
// PTY master (so ^C and other control bytes hit the guest line discipline),
// master output emits as stdout events, and TtyResize frames resize the
// terminal while the command runs.
func (h *Handler) runExecTTY(ctx context.Context, cmd *exec.Cmd, stdin io.Reader, stdout io.Writer) (int, error) {
	resizeCh := make(chan TtyResize, 4)
	if sr, ok := stdin.(*execStdinReader); ok {
		sr.resize = resizeCh
	}

	ptmx, err := pty.Start(cmd)
	if err != nil {
		return 1, err
	}
	defer ptmx.Close()

	copyDone := make(chan struct{})
	go func() {
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

	go func() {
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

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-resizeCh:
				if !ok {
					return
				}
				_ = pty.Setsize(ptmx, &pty.Winsize{Cols: uint16(ev.Cols), Rows: uint16(ev.Rows)})
			}
		}
	}()

	err = cmd.Wait()
	<-copyDone
	if err == nil {
		return 0, nil
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	return 1, err
}
