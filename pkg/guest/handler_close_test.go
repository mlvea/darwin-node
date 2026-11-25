package guest

import (
	"context"
	"net"
	"testing"
	"time"
)

// Handler.Close must end Serve while the host still holds the client.
// Leaving the idle watchdog parked until IdleTimeout is the leak the
// engine tests report after a VM stop.
func TestHandlerCloseStopsServe(t *testing.T) {
	h := Handler{Token: "tok", IdleTimeout: time.Hour}
	h.Init()
	agent, host := net.Pipe()
	done := make(chan struct{})
	go func() {
		_ = Serve(context.Background(), agent, h)
		close(done)
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cli, err := Dial(ctx, host, "tok", "host")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	h.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Serve still running after Handler.Close")
	}
}
