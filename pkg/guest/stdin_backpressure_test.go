package guest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
)

// A slow exec must receive every stdin byte and then EOF. The upstream
// queue used to hold 64 frames and, once full, drop the rest. The EOF
// marker was dropped with them, so the command never finished.
func TestSlowStdinKeepsEveryByteAndEOF(t *testing.T) {
	leakcheck.Check(t)
	const size = 4 << 20
	payload := bytes.Repeat([]byte("x"), size)
	h := Handler{
		Token: "tok",
		ExecFn: func(ctx context.Context, req ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			// Hold the reader long enough for the host to fill the old
			// 64-frame queue. A current host waits; it does not drop.
			time.Sleep(150 * time.Millisecond)
			got, err := io.ReadAll(stdin)
			if err != nil {
				return 1, err
			}
			if len(got) != len(payload) || !bytes.Equal(got, payload) {
				return 2, fmt.Errorf("stdin delivered %d bytes, want %d", len(got), len(payload))
			}
			return 0, nil
		},
	}
	cli, _ := serveTestHandler(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	code, err := cli.ExecInteractive(ctx, ExecReq{Argv: []string{"cat"}}, bytes.NewReader(payload), nil, io.Discard, io.Discard)
	if err != nil || code != 0 {
		t.Fatalf("exec code %d err %v; stdin was dropped or EOF never arrived", code, err)
	}
}
