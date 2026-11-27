//go:build linux || darwin

package supervisor

import (
	"errors"
	"os/exec"
	"testing"
)

func TestProcessIdentityFence(t *testing.T) {
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

	lease := Lease{SupervisorPID: cmd.Process.Pid}
	if err := AnnotateProcess(&lease); err != nil {
		t.Fatal(err)
	}

	t.Run("live", func(t *testing.T) {
		if err := CheckProcessLease(lease); err != nil {
			t.Fatal(err)
		}
		boot, err := HostBootID()
		if err != nil {
			t.Fatal(err)
		}
		if boot == "" || lease.HostBootID != boot {
			t.Fatalf("host boot %q, lease %q", boot, lease.HostBootID)
		}
		start, err := ProcessStart(cmd.Process.Pid)
		if err != nil {
			t.Fatal(err)
		}
		if lease.SupervisorProcStart != start {
			t.Fatalf("os start %d, lease %d", start, lease.SupervisorProcStart)
		}
	})
	t.Run("boot", func(t *testing.T) {
		bad := lease
		bad.HostBootID = "not-this-boot"
		if err := CheckProcessLease(bad); !errors.Is(err, ErrFence) {
			t.Fatalf("got %v, want fence rejection", err)
		}
	})
	t.Run("reused", func(t *testing.T) {
		bad := lease
		bad.SupervisorProcStart++
		if err := CheckProcessLease(bad); !errors.Is(err, ErrFence) {
			t.Fatalf("got %v, want fence rejection for a reused pid", err)
		}
	})
	t.Run("dead", func(t *testing.T) {
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		<-done
		if err := CheckProcessLease(lease); !errors.Is(err, ErrFence) {
			t.Fatalf("got %v, want fence rejection for a dead pid", err)
		}
	})
}
