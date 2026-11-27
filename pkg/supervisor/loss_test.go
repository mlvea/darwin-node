//go:build linux || darwin

package supervisor

import (
	"net"
	"os/exec"
	"testing"
	"time"
)

func TestESRCHMarksLostOnce(t *testing.T) {
	ctrlR, ctrlW := unixPair(t)
	guestR, guestW := unixPair(t)
	defer func() { _ = ctrlR.Close() }()
	defer func() { _ = ctrlW.Close() }()
	defer func() { _ = guestR.Close() }()
	defer func() { _ = guestW.Close() }()

	n := 0
	stop, err := watchLoss(1<<30, rawUnixFD(t, ctrlR), rawUnixFD(t, guestR), func() { n++ })
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if n != 1 {
		t.Fatalf("lost calls %d, want 1 for ESRCH before the socket closes", n)
	}
}

func TestProcExitAndCtrlEOFLoseOnce(t *testing.T) {
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

	ctrlR, ctrlW := unixPair(t)
	guestR, guestW := unixPair(t)
	got := make(chan struct{}, 2)
	stop, err := watchLoss(cmd.Process.Pid, rawUnixFD(t, ctrlR), rawUnixFD(t, guestR), func() {
		got <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		stop()
		_ = ctrlR.Close()
		_ = ctrlW.Close()
		_ = guestR.Close()
		_ = guestW.Close()
	}()

	if err := ctrlW.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("process exit and control EOF did not mark the supervisor lost")
	}
	time.Sleep(150 * time.Millisecond)
	if extra := len(got); extra != 0 {
		t.Fatalf("supervisor lost fired %d extra times", extra)
	}
}

func rawUnixFD(t *testing.T, c *net.UnixConn) int {
	t.Helper()
	raw, err := c.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var fd int
	if err := raw.Control(func(f uintptr) { fd = int(f) }); err != nil {
		t.Fatal(err)
	}
	return fd
}
