package simvz

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
)

// guestProc is one darwin-guest-agent child. kill is safe to call after
// the process has already exited.
type guestProc struct {
	cmd     *exec.Cmd
	done    chan struct{}
	waitErr error
	out     *safeBuf
}

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) {
	if s == nil {
		return len(p), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	const max = 8192
	if s.b.Len() < max {
		room := max - s.b.Len()
		if len(p) > room {
			_, _ = s.b.Write(p[:room])
		} else {
			_, _ = s.b.Write(p)
		}
	}
	return len(p), nil
}

func (s *safeBuf) String() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func (p *guestProc) kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Kill()
	<-p.done
}

func guestAgentEnv() []string {
	return append(os.Environ(), guest.EnvGuestSoftwarePlatform+"=1")
}

func (m *machine) spawnGuest(startCtx context.Context) error {
	path, err := shortSock("simvz")
	if err != nil {
		return err
	}
	args := []string{"--unix-listen", path}
	if m.token != "" {
		args = append(args, "--token", m.token)
	} else {
		args = append(args, "--insecure-no-token")
	}
	if m.rt.idle > 0 && m.rt.idle != defaultIdle {
		args = append(args, "--idle-timeout", m.rt.idle.String())
	}
	cmd := exec.Command(m.rt.guestBin, args...)
	// The child is this Mac, not a guest. Without this it would write
	// /dev/random, set the clock, rename the machine, and delete SSH host
	// keys on the runner. The image's launchd job does not set the variable.
	cmd.Env = guestAgentEnv()
	setDeathSignal(cmd)
	out := &safeBuf{}
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("simvz: start guest agent: %w", err)
	}
	proc := &guestProc{cmd: cmd, done: make(chan struct{}), out: out}
	go func() {
		proc.waitErr = cmd.Wait()
		close(proc.done)
	}()
	m.mu.Lock()
	m.sock = path
	m.proc = proc
	m.bindGenLocked()
	m.mu.Unlock()
	if err := waitListen(startCtx, path, proc.done, proc.out); err != nil {
		proc.kill()
		return err
	}
	m.mu.Lock()
	m.agentReady = true
	m.bindGenLocked()
	m.mu.Unlock()
	return nil
}

func waitListen(ctx context.Context, path string, died <-chan struct{}, out *safeBuf) error {
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	var last error
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-died:
			return fmt.Errorf("simvz: guest agent exited before listening: %s", out.String())
		case <-deadline.C:
			if last == nil {
				last = fmt.Errorf("timed out")
			}
			return fmt.Errorf("simvz: guest agent did not listen on %s: %w (%s)", path, last, out.String())
		case <-tick.C:
			c, err := net.Dial("unix", path)
			if err != nil {
				last = err
				continue
			}
			_ = c.Close()
			return nil
		}
	}
}
