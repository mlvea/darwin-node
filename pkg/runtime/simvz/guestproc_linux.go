//go:build linux

package simvz

import (
	"os/exec"
	"syscall"
)

// setDeathSignal kills the guest if this process dies between Start and
// Stop, so a panicked test does not leave darwin-guest-agent behind.
func setDeathSignal(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
}
