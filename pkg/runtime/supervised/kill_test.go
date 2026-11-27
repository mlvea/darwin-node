//go:build linux || darwin

package supervised

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/supervisor"
	"golang.org/x/sys/unix"
)

func TestAgentStopBudget(t *testing.T) {
	if got := agentStopBudget(0, 0); got != 12*time.Second {
		t.Fatalf("agent stop budget %s, want 12s", got)
	}
	if got := agentStopBudget(4*time.Second, 0); got != 16*time.Second {
		t.Fatalf("agent stop budget with grace %s, want 16s", got)
	}
	if got := agentStopBudget(4*time.Second, 25*time.Millisecond); got != 4*time.Second+25*time.Millisecond {
		t.Fatalf("override budget %s", got)
	}
}

func TestEnforceStopKillsAndWaits(t *testing.T) {
	cmd, done := startSleep(t)
	ident := liveIdentity(t, cmd.Process.Pid)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	err := enforceStop(ctx, 40*time.Millisecond, ident, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("stop budget was not enforced, took %s", time.Since(started))
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("killed supervisor was not reaped")
	}
	if processAlive(cmd.Process.Pid) {
		t.Fatal("supervisor was still alive when the slot would be released")
	}
	if err == nil {
		t.Fatal("kill path returned a nil error")
	}
}

func TestEnforceStopDoesNotKillReusedPID(t *testing.T) {
	cmd, done := startSleep(t)
	ident := liveIdentity(t, cmd.Process.Pid)
	ident.Start++
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	err := enforceStop(ctx, 40*time.Millisecond, ident, func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if time.Since(started) > 500*time.Millisecond {
		t.Fatalf("stop budget was not enforced, took %s", time.Since(started))
	}
	if !processAlive(cmd.Process.Pid) {
		t.Fatal("reused pid was killed")
	}
	if !errors.Is(err, supervisor.ErrFence) {
		t.Fatalf("got %v, want fence rejection", err)
	}
	<-done
}

func startSleep(t *testing.T) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	bin, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
	})
	return cmd, done
}

func liveIdentity(t *testing.T, pid int) supervisor.ProcessIdentity {
	t.Helper()
	lease := supervisor.Lease{SupervisorPID: pid}
	if err := supervisor.AnnotateProcess(&lease); err != nil {
		t.Fatal(err)
	}
	return supervisor.ProcessIdentity{
		PID:    pid,
		Start:  lease.SupervisorProcStart,
		BootID: lease.HostBootID,
	}
}

func processAlive(pid int) bool {
	return unix.Kill(pid, 0) == nil
}
