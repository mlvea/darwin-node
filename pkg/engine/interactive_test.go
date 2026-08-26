package engine

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	api "github.com/virtual-kubelet/virtual-kubelet/node/api"
	corev1 "k8s.io/api/core/v1"
)

// lockedBuffer is an io.Writer safe for concurrent use.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// attachIO implements api.AttachIO for tests.
type attachIO struct {
	stdinR io.Reader
	out    *lockedBuffer
	tty    bool
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

func (a *attachIO) Stdin() io.Reader { return a.stdinR }
func (a *attachIO) Stdout() io.WriteCloser {
	if a.out == nil {
		return nopWriteCloser{io.Discard}
	}
	return nopWriteCloser{a.out}
}
func (a *attachIO) Stderr() io.WriteCloser      { return nopWriteCloser{io.Discard} }
func (a *attachIO) TTY() bool                   { return a.tty }
func (a *attachIO) Resize() <-chan api.TermSize { return nil }

func newInteractiveEngine(t *testing.T, cfg config.Config, rt *fake.Runtime) *Engine {
	t.Helper()
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)
	return e
}

func TestExecStdinThroughEngine(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	rt := fake.New()
	rt.ExecFn = func(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		if !req.Stdin {
			return 1, nil
		}
		_, err := io.Copy(stdout, stdin)
		if err != nil && err != io.EOF {
			return 1, err
		}
		return 0, nil
	}
	e := newInteractiveEngine(t, cfg, rt)

	if err := e.Create(context.Background(), samplePod("ix", "uid-ix"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "ix", corev1.PodRunning)

	stdinR, stdinW := io.Pipe()
	out := &lockedBuffer{}
	att := &attachIO{stdinR: stdinR, out: out}

	errCh := make(chan error, 1)
	go func() {
		errCh <- e.ExecInVM(context.Background(), "default", "ix", []string{"cat"}, att)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && out.String() == "" {
		if _, err := stdinW.Write([]byte("engine-echo\n")); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := out.String(); !bytes.Contains([]byte(got), []byte("engine-echo")) {
		t.Fatalf("no echo: %q", got)
	}
	_ = stdinW.Close()
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	_ = e.Delete(context.Background(), "default", "ix", 0)
}

func TestConsoleSocketRoundTrip(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	cfg.SerialConsole = true
	rt := fake.New()
	e := newInteractiveEngine(t, cfg, rt)

	pod := samplePod("cs", "uid-cs")
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "cs", corev1.PodRunning)

	sock := ConsoleSocketPath("default@cs")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial console sock: %v", err)
	}
	defer conn.Close()

	guestEnd := rt.ConsoleGuestEnd()
	if guestEnd == nil {
		t.Fatal("fake console guest end missing")
	}
	go func() {
		buf := make([]byte, 1)
		for {
			if _, err := guestEnd.Read(buf); err != nil {
				return
			}
			_, _ = guestEnd.Write([]byte{'Z'})
		}
	}()

	if _, err := conn.Write([]byte{0x03}); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 1)
	if _, err := conn.Read(buf); err != nil || buf[0] != 'Z' {
		t.Fatalf("console round trip: %v %v", buf, err)
	}
	_ = e.Delete(context.Background(), "default", "cs", 0)
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatalf("socket not removed after delete: %v", err)
	}
}
