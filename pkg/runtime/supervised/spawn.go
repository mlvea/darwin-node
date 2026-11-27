package supervised

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
)

// Proc is a supervisor child that has been detached from this process
// group. Kill is for tests. Production leaves the child to init when
// the agent exits, and reaps it when the agent is still alive.
type Proc struct {
	cmd  *exec.Cmd
	done chan struct{}
}

// Kill signals the child and waits until it has been reaped.
func (p *Proc) Kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	<-p.done
}

func (r *Runtime) spawnSlot(slot int) (*Proc, error) {
	return spawnProcess(r.binary(), r.supervisorArgs(slot), supervisorLog(r.RunDir, slot))
}

func supervisorLog(runDir string, slot int) string {
	return filepath.Join(runDir, fmt.Sprintf("slot-%d.supervisor.log", slot))
}

// spawnProcess starts binary in a new session. Setsid plus abandoning the
// process group is the dev stand-in for a launchd LaunchAgent. There is
// no Pdeathsig: a dead agent must not take the VM with it. The Wait
// goroutine reaps the child when this process outlives it.
func spawnProcess(binary string, args []string, logPath string) (*Proc, error) {
	cmd := exec.Command(binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p := &Proc{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}
