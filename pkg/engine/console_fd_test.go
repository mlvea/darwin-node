package engine

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/types"
)

// consolePipeMachine serves one real OS pipe as the serial console.
// The fake runtime's Console is a net.Pipe and does not hold an fd.
type consolePipeMachine struct {
	runtime.Machine
	cons io.ReadWriteCloser
}

func (m consolePipeMachine) Console() (io.ReadWriteCloser, error) {
	return m.cons, nil
}

func descriptorOpen(fd int) bool {
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), uintptr(syscall.F_GETFD), 0)
	return errno == 0
}

func newConsolePipe(t *testing.T) (r *os.File, fd int) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = w.Close()
		_ = r.Close()
	})
	raw := r.Fd()
	if raw == ^uintptr(0) {
		t.Fatal("console pipe has no fd")
	}
	return r, int(raw)
}

func TestServeConsoleClosesFDWhenListenFails(t *testing.T) {
	leakcheck.Check(t)
	r, fd := newConsolePipe(t)
	pod := samplePod("console-listen", "uid-console-listen")
	sock := ConsoleSocketPath(pod.Namespace, pod.Name)
	if err := os.RemoveAll(sock); err != nil {
		t.Fatal(err)
	}
	// os.Remove deletes an empty directory, which would let Listen succeed.
	// A file inside the directory keeps the name occupied, so Listen fails.
	if err := os.MkdirAll(sock, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sock, "busy"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sock) })

	m, err := fake.New().Create(context.Background(), types.VMSpec{})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{cfg: config.Config{SerialConsole: true}, events: event.Nop{}}
	rec := &podRecord{pod: pod, machine: consolePipeMachine{Machine: m, cons: r}}
	e.serveConsole(rec)
	if descriptorOpen(fd) {
		t.Fatal("serial console fd still open after Listen failed")
	}
	if rec.consoleLn != nil {
		t.Fatal("console listener set after Listen failed")
	}
}

func TestServeConsoleClosesFDWhenChmodFails(t *testing.T) {
	leakcheck.Check(t)
	r, fd := newConsolePipe(t)
	pod := samplePod("console-chmod", "uid-console-chmod")
	sock := ConsoleSocketPath(pod.Namespace, pod.Name)
	_ = os.Remove(sock)
	t.Cleanup(func() { _ = os.Remove(sock) })

	m, err := fake.New().Create(context.Background(), types.VMSpec{})
	if err != nil {
		t.Fatal(err)
	}
	e := &Engine{cfg: config.Config{SerialConsole: true}, events: event.Nop{}}
	rec := &podRecord{pod: pod, machine: consolePipeMachine{Machine: m, cons: r}}
	e.serveConsoleAt(rec, func(path string) (net.Listener, error) {
		return net.Listen("unix", path)
	}, func(string, os.FileMode) error {
		return errors.New("chmod failed")
	})
	if descriptorOpen(fd) {
		t.Fatal("serial console fd still open after Chmod failed")
	}
	if rec.consoleLn != nil {
		t.Fatal("console listener kept after Chmod failed")
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Fatalf("console socket left behind: %v", err)
	}
}
