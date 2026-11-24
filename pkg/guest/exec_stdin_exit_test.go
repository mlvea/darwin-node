package guest

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
)

// A non-TTY exec whose process exits without reading stdin must return
// and free its exec slot. cmd.Run waits for the stdin copy, and that copy
// stays blocked in Read when the client has not sent EOF.
func TestNonTTYExecWithStdinReturnsWhenProcessExits(t *testing.T) {
	leakcheck.Check(t)
	h := Handler{Token: "tok"}
	cli, _ := serveTestHandler(t, h)

	pr, pw := io.Pipe()
	t.Cleanup(func() { _ = pw.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	code, err := cli.ExecInteractive(ctx, ExecReq{Argv: []string{"/bin/sh", "-c", "exit 0"}}, pr, nil, io.Discard, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("exec code %d err %v after %s; stdin copy outlived the process", code, err, time.Since(start))
	}
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("exec returned in %s; the slot was held until the caller gave up", time.Since(start))
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	stdout, _, code, err := cli.Exec(ctx2, ExecReq{Argv: []string{"/bin/sh", "-c", "exit 0"}})
	if err != nil || code != 0 {
		t.Fatalf("next exec code %d err %v stdout %q; the previous slot is still held", code, err, stdout)
	}
}
