package sidecar

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/docker/docker/api/types"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
)

// execDocker serves one container exec. The daemon reports the exit code
// from ContainerExecInspect after the attach stream ends.
type execDocker struct {
	scriptedDocker
	stdout     string
	exitCode   int
	running    bool
	inspectErr error
	inspected  int
}

func (f *execDocker) ContainerExecCreate(context.Context, string, types.ExecConfig) (types.IDResponse, error) {
	return types.IDResponse{ID: "exec-1"}, nil
}

func (f *execDocker) ContainerExecAttach(context.Context, string, types.ExecStartCheck) (types.HijackedResponse, error) {
	left, right := net.Pipe()
	_ = right.Close()
	return types.HijackedResponse{Conn: left, Reader: bufio.NewReader(strings.NewReader(f.stdout))}, nil
}

func (f *execDocker) ContainerExecInspect(context.Context, string) (types.ContainerExecInspect, error) {
	f.inspected++
	if f.inspectErr != nil {
		return types.ContainerExecInspect{}, f.inspectErr
	}
	return types.ContainerExecInspect{ExecID: "exec-1", ExitCode: f.exitCode, Running: f.running}, nil
}

type bufAttach struct {
	out bytes.Buffer
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }

func (a *bufAttach) Stdin() io.Reader            { return nil }
func (a *bufAttach) Stdout() io.WriteCloser      { return nopCloser{&a.out} }
func (a *bufAttach) Stderr() io.WriteCloser      { return nopCloser{io.Discard} }
func (a *bufAttach) TTY() bool                   { return false }
func (a *bufAttach) Resize() <-chan api.TermSize { return nil }

func TestDockerExecSurfacesExitCode(t *testing.T) {
	t.Run("nonzero", func(t *testing.T) {
		cli := &execDocker{stdout: "nope\n", exitCode: 7}
		d := &Docker{cli: cli, ids: map[string]map[string]string{"ns/p": {"log": "abc"}}}
		attach := &bufAttach{}
		err := d.Exec(context.Background(), "ns", "p", "log", []string{"false"}, attach)
		if err == nil || !strings.Contains(err.Error(), "7") {
			t.Fatalf("err %v, exec exit 7 was not surfaced", err)
		}
		if attach.out.String() != "nope\n" {
			t.Fatalf("stdout %q", attach.out.String())
		}
		if cli.inspected != 1 {
			t.Fatalf("inspect calls %d", cli.inspected)
		}
	})

	t.Run("zero", func(t *testing.T) {
		cli := &execDocker{stdout: "ok\n", exitCode: 0}
		d := &Docker{cli: cli, ids: map[string]map[string]string{"ns/p": {"log": "abc"}}}
		attach := &bufAttach{}
		if err := d.Exec(context.Background(), "ns", "p", "log", []string{"true"}, attach); err != nil {
			t.Fatal(err)
		}
		if attach.out.String() != "ok\n" {
			t.Fatalf("stdout %q", attach.out.String())
		}
		if cli.inspected != 1 {
			t.Fatalf("inspect calls %d", cli.inspected)
		}
	})

	t.Run("inspect error", func(t *testing.T) {
		cli := &execDocker{inspectErr: errors.New("daemon inspect failed")}
		d := &Docker{cli: cli, ids: map[string]map[string]string{"ns/p": {"log": "abc"}}}
		err := d.Exec(context.Background(), "ns", "p", "log", []string{"true"}, nil)
		if err == nil || !strings.Contains(err.Error(), "daemon inspect failed") {
			t.Fatalf("err %v, inspect failure was swallowed", err)
		}
	})

	t.Run("still running", func(t *testing.T) {
		cli := &execDocker{running: true, exitCode: 0}
		d := &Docker{cli: cli, ids: map[string]map[string]string{"ns/p": {"log": "abc"}}}
		err := d.Exec(context.Background(), "ns", "p", "log", []string{"sleep"}, nil)
		if err == nil {
			t.Fatal("exec still running returned nil")
		}
	})
}
