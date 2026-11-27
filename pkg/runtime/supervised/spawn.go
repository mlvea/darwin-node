package supervised

import (
	"fmt"
	"os"
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
	if r.RunDir != "" {
		if err := os.MkdirAll(r.RunDir, 0o700); err != nil {
			return nil, err
		}
	}
	return spawnProcess(r.binary(), r.supervisorArgs(slot), supervisorLog(r.RunDir, slot))
}

func supervisorLog(runDir string, slot int) string {
	return filepath.Join(runDir, fmt.Sprintf("slot-%d.supervisor.log", slot))
}

// spawnProcess starts binary in a new session with file-backed stdio.
// Setsid plus abandoning the process group is the dev stand-in for a
// launchd LaunchAgent. There is no Pdeathsig: a dead agent must not take
// the VM with it. Stdin is /dev/null and stdout/stderr append to logPath,
// so the agent's exit cannot deliver SIGPIPE on a pipe. The Wait
// goroutine reaps the child when this process outlives it. The parent
// copies of those files are closed after Start.
func spawnProcess(binary string, args []string, logPath string) (*Proc, error) {
	if logPath == "" {
		return nil, fmt.Errorf("supervisor log path is empty")
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	null, err := os.Open(os.DevNull)
	if err != nil {
		_ = logf.Close()
		return nil, err
	}
	cmd := exec.Command(binary, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Stdin = null
	cmd.Stdout = logf
	cmd.Stderr = logf
	if err := cmd.Start(); err != nil {
		_ = null.Close()
		_ = logf.Close()
		return nil, err
	}
	_ = null.Close()
	_ = logf.Close()
	p := &Proc{cmd: cmd, done: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(p.done)
	}()
	return p, nil
}
