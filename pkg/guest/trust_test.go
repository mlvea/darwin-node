package guest

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"
)

func TestReadFrameRejectsOversizedControl(t *testing.T) {
	body := controlFrame(t, MethodHealth, strings.Repeat("a", 70<<10))
	_, err := ReadFrame(bytes.NewReader(body))
	if err == nil {
		t.Fatal("control frame over 64KiB was accepted")
	}
}

func TestReadFrameAllowsLargeStream(t *testing.T) {
	body := frameWithKind(t, KindStream, MethodExec, strings.Repeat("a", 70<<10))
	if _, err := ReadFrame(bytes.NewReader(body)); err != nil {
		t.Fatalf("stream frame over 64KiB was rejected: %v", err)
	}
}

func TestReadFrameAllowsLargeMaterialize(t *testing.T) {
	body := controlFrame(t, MethodMaterialize, strings.Repeat("a", 70<<10))
	if _, err := ReadFrame(bytes.NewReader(body)); err != nil {
		t.Fatalf("materialize frame over 64KiB was rejected: %v", err)
	}
}

func TestReadFrameRejectsDeepJSON(t *testing.T) {
	nested := "0"
	for i := 0; i < 40; i++ {
		nested = `{"a":` + nested + `}`
	}
	raw := []byte(`{"v":1,"id":"1","kind":"req","method":"Health","payload":` + nested + `}`)
	body := prefixLen(raw)
	if _, err := ReadFrame(bytes.NewReader(body)); err == nil {
		t.Fatal("deeply nested frame was accepted")
	}
}

func TestOversizeControlClosesSession(t *testing.T) {
	a, b := net.Pipe()
	defer b.Close()
	fc := NewFrameConn(a)
	body := controlFrame(t, MethodHealth, strings.Repeat("b", 70<<10))
	go func() { _, _ = b.Write(body) }()
	if _, err := fc.Read(); err == nil {
		t.Fatal("oversized control frame was accepted")
	}
	_ = b.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	if _, err := b.Read(make([]byte, 1)); err == nil {
		t.Fatal("session stayed open after an oversized control frame")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("session stayed open after an oversized control frame")
	}
}

func TestHostHandshakeNegotiatesProtocol(t *testing.T) {
	a, b := net.Pipe()
	got := make(chan Envelope, 1)
	go func() {
		env, err := ReadFrame(a)
		if err != nil {
			return
		}
		got <- env
		res, _ := EncodePayload(HandshakeRes{OK: true, AgentVersion: "old", Protocol: ProtocolVersion})
		_ = WriteFrame(a, Envelope{V: ProtocolVersion, ID: env.ID, Kind: KindResponse, Method: MethodHandshake, Payload: res})
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	type dialed struct {
		c   *Client
		err error
	}
	done := make(chan dialed, 1)
	go func() {
		c, err := Dial(ctx, b, "tok", "darwin-node")
		done <- dialed{c, err}
	}()
	select {
	case env := <-got:
		var req map[string]any
		if err := json.Unmarshal(env.Payload, &req); err != nil {
			t.Fatal(err)
		}
		if req["minProtocol"] == nil || req["maxProtocol"] == nil || req["capabilities"] == nil {
			t.Fatalf("host handshake did not negotiate: %s", env.Payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no handshake")
	}
	select {
	case d := <-done:
		if d.err != nil {
			t.Fatal(d.err)
		}
		t.Cleanup(func() { _ = d.c.Close() })
		if d.c.Protocol() != ProtocolVersion {
			t.Fatalf("protocol %d", d.c.Protocol())
		}
		if len(d.c.Capabilities()) != 0 || d.c.HasCapability(CapMetrics) {
			t.Fatalf("old agent capabilities = %v", d.c.Capabilities())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("dial did not finish")
	}
}

func TestGuestHandshakeListsCapabilities(t *testing.T) {
	a, b := net.Pipe()
	h := Handler{Token: "secret", AgentVersion: "test"}
	go func() { _ = Serve(context.Background(), a, h) }()
	defer b.Close()
	req, _ := EncodePayload(map[string]any{
		"token":        "secret",
		"nonce":        "once",
		"minProtocol":  1,
		"maxProtocol":  1,
		"capabilities": []string{"metrics", "not-a-real-cap"},
		"futureField":  map[string]any{"nested": true},
	})
	if err := WriteFrame(b, Envelope{V: ProtocolVersion, ID: "hs", Kind: KindRequest, Method: MethodHandshake, Payload: req}); err != nil {
		t.Fatal(err)
	}
	_ = b.SetReadDeadline(time.Now().Add(3 * time.Second))
	env, err := ReadFrame(b)
	if err != nil {
		t.Fatal(err)
	}
	if env.Kind == KindError {
		t.Fatalf("unknown JSON field rejected the handshake: %+v", env.Error)
	}
	var res map[string]any
	if err := json.Unmarshal(env.Payload, &res); err != nil {
		t.Fatal(err)
	}
	if res["ok"] != true {
		t.Fatalf("handshake %+v", res)
	}
	caps, _ := res["capabilities"].([]any)
	if len(caps) == 0 || res["protocol"] == nil || res["agentVersion"] == nil {
		t.Fatalf("guest did not advertise protocol, version, and capabilities: %s", env.Payload)
	}
}

func TestDefaultMetricsMeasuresMemory(t *testing.T) {
	m, err := defaultMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if m.MemoryWorkingSet == 0 || m.MemoryRSS == 0 {
		t.Fatalf("guest metrics CPU=%d workingSet=%d rss=%d; CPU and memory are still unmeasured", m.CPUNanoCores, m.MemoryWorkingSet, m.MemoryRSS)
	}
}

func controlFrame(t *testing.T, method, payload string) []byte {
	t.Helper()
	return frameWithKind(t, KindRequest, method, payload)
}

func frameWithKind(t *testing.T, kind Kind, method, payload string) []byte {
	t.Helper()
	env := map[string]any{
		"v":       ProtocolVersion,
		"id":      "1",
		"kind":    kind,
		"method":  method,
		"payload": payload,
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	return prefixLen(raw)
}

func prefixLen(raw []byte) []byte {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(raw)))
	return append(hdr[:], raw...)
}
