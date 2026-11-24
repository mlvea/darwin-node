//go:build unix

package guest

import (
	"os/exec"
	"syscall"
)

// processExitCode reports the container exit code for a wait error.
// A process killed by signal N is 128+N, the same convention kubelet uses.
func processExitCode(err error) int32 {
	if err == nil {
		return 0
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		return 1
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int32(ws.Signal())
	}
	code, _ := exitCodeFromError(err)
	return code
}
