package guest

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"testing"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
)

// An exec stream that finishes without an Exited frame must not report
// success. Exit code 0 is a real success only when the guest said so.

func TestExecStreamWithoutExitFrameIsAnError(t *testing.T) {
	leakcheck.Check(t)
	host, agent := net.Pipe()
	go serveExecFrames(agent, ExecEvent{Stdout: []byte("partial")}, false, 0)
	cli, err := Dial(context.Background(), host, "tok", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	var out bytes.Buffer
	code, err := cli.ExecStream(context.Background(), ExecReq{Argv: []string{"cat"}}, &out, io.Discard)
	if err == nil || code == 0 {
		t.Fatalf("exit %d err %v, stream without an exit frame reported success", code, err)
	}
	if !strings.Contains(err.Error(), "exit") {
		t.Fatalf("err %v, want an unknown-exit error", err)
	}
	if !bytes.Contains(out.Bytes(), []byte("partial")) {
		t.Fatalf("stdout %q, bytes before the missing exit must still be delivered", out.Bytes())
	}
}

func TestExecStreamExitFrameKeepsCode(t *testing.T) {
	leakcheck.Check(t)
	host, agent := net.Pipe()
	go serveExecFrames(agent, ExecEvent{Stdout: []byte("bye")}, true, 7)
	cli, err := Dial(context.Background(), host, "tok", "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	var out bytes.Buffer
	code, err := cli.ExecStream(context.Background(), ExecReq{Argv: []string{"cat"}}, &out, io.Discard)
	if err != nil || code != 7 {
		t.Fatalf("exit %d err %v, want 7", code, err)
	}
	if out.String() != "bye" {
		t.Fatalf("stdout %q", out.String())
	}
}

// serveExecFrames answers Handshake, then for Exec writes one stdout stream
// frame and a terminal response. The connection stays open so the client
// can read those frames; closing it here races the session's closed
// select and drops them. exited false omits the exit frame.
func serveExecFrames(rw io.ReadWriteCloser, stdout ExecEvent, exited bool, exitCode int) {
	defer rw.Close()
	for {
		env, err := ReadFrame(rw)
		if err != nil {
			return
		}
		switch env.Method {
		case MethodHandshake:
			if err := writeExecResult(rw, env, HandshakeRes{OK: true, AgentVersion: "test", Protocol: ProtocolVersion}); err != nil {
				return
			}
		case MethodExec:
			body, err := EncodePayload(stdout)
			if err != nil {
				return
			}
			if err := WriteFrame(rw, Envelope{
				V: ProtocolVersion, ID: env.ID, Kind: KindStream, Method: MethodExec, Payload: body,
			}); err != nil {
				return
			}
			var payload any
			if exited {
				payload = ExecEvent{Exited: true, ExitCode: exitCode}
				exitBody, err := EncodePayload(payload)
				if err != nil {
					return
				}
				if err := WriteFrame(rw, Envelope{
					V: ProtocolVersion, ID: env.ID, Kind: KindStream, Method: MethodExec, Payload: exitBody,
				}); err != nil {
					return
				}
			}
			if err := writeExecResult(rw, env, payload); err != nil {
				return
			}
		default:
			if err := WriteFrame(rw, Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: env.Method}); err != nil {
				return
			}
		}
	}
}

func writeExecResult(rw io.Writer, env Envelope, payload any) error {
	body, err := EncodePayload(payload)
	if err != nil {
		return err
	}
	return WriteFrame(rw, Envelope{
		V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: env.Method, Payload: body,
	})
}
