package guest

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestSelectProtocol(t *testing.T) {
	got, err := selectProtocol(0, 0)
	if err != nil || got != ProtocolVersion {
		t.Fatalf("legacy host: %d %v", got, err)
	}
	got, err = selectProtocol(1, 1)
	if err != nil || got != 1 {
		t.Fatalf("v1: %d %v", got, err)
	}
	if _, err := selectProtocol(2, 3); err == nil {
		t.Fatal("guest accepted a protocol it does not speak")
	}
}

func TestIntersectCapabilities(t *testing.T) {
	got := IntersectCapabilities([]string{"metrics", "exit"}, []string{"exit", "workload", "exit"})
	if strings.Join(got, ",") != "exit" {
		t.Fatalf("%v", got)
	}
	if IntersectCapabilities([]string{"metrics"}, nil) != nil {
		t.Fatal("old guest must yield an empty set")
	}
}

func TestDialIntersectsCapabilities(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	go func() {
		env, err := ReadFrame(a)
		if err != nil {
			return
		}
		res, _ := EncodePayload(HandshakeRes{
			OK:           true,
			AgentVersion: "new",
			Protocol:     ProtocolVersion,
			Capabilities: []string{CapMetrics, "not-a-real-cap", CapMetrics},
		})
		_ = WriteFrame(a, Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: MethodHandshake, Payload: res})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := Dial(ctx, b, "tok", "darwin-node")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if !c.HasCapability(CapMetrics) {
		t.Fatal("metrics was on both lists")
	}
	if c.HasCapability(CapWorkload) || c.HasCapability("not-a-real-cap") {
		t.Fatalf("intersection = %v", c.Capabilities())
	}
	if got := strings.Join(c.Capabilities(), ","); got != CapMetrics {
		t.Fatalf("capabilities %q", got)
	}
}
