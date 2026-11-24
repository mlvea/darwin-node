package guest

import (
	"context"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
)

// A session with no probes and no exec is idle. The guest closes it after
// IdleTimeout. The host must send a heartbeat well under that window.
func TestHostHeartbeatKeepsNonInteractiveSession(t *testing.T) {
	leakcheck.Check(t)
	h := Handler{Token: "tok", IdleTimeout: 300 * time.Millisecond}
	cli, _ := serveTestHandler(t, h)

	time.Sleep(800 * time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	res, err := cli.Health(ctx)
	if err != nil || !res.OK {
		t.Fatalf("session closed at the idle timeout and the host sent no heartbeat: %v %+v", err, res)
	}
}
