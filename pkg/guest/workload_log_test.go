package guest

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/internal/leakcheck"
)

func TestSuperviseWritesWorkloadOutputToLogBuffer(t *testing.T) {
	leakcheck.Check(t)
	h := &Handler{LogBuffer: NewLogBuffer(32)}
	h.Init()
	err := Supervise(context.Background(), h, []string{"/bin/sh", "-c", "echo hello-workload"}, time.Second, func() {})
	if err != nil {
		t.Fatal(err)
	}
	joined := bytes.Join(h.LogBuffer.Tail(0), nil)
	if !bytes.Contains(joined, []byte("hello-workload")) {
		t.Fatalf("workload stdout was discarded: %q", joined)
	}
}

func TestLogsTimestampsPrefixLines(t *testing.T) {
	leakcheck.Check(t)
	h := Handler{Token: "tok", LogBuffer: NewLogBuffer(8)}
	if _, err := h.LogBuffer.Write([]byte("hello-log\n")); err != nil {
		t.Fatal(err)
	}
	cli, _ := serveTestHandler(t, h)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	lines, err := cli.Logs(ctx, LogsReq{Timestamps: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || !bytes.Contains(lines[0], []byte("hello-log")) {
		t.Fatalf("lines %q", lines)
	}
	line := string(lines[0])
	if !strings.Contains(line, "T") || !strings.Contains(line, "Z ") {
		t.Fatalf("kubectl logs --timestamps returned %q", line)
	}
}
