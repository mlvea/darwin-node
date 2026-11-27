package supervisor

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"os"
	"testing"
	"time"
)

func writeRaw(conn net.Conn, body []byte) error {
	buf := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(buf[:4], uint32(len(body)))
	copy(buf[4:], body)
	_, err := conn.Write(buf)
	return err
}

func readRaw(conn net.Conn) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	body := make([]byte, n)
	_, err := io.ReadFull(conn, body)
	return body, err
}

// TestUnixHandshakeRoundTrip dials a real supervisor socket and checks
// the version fields, a minor the agent has not seen, a supervisor
// that predates the fields, and the workload note.
func TestUnixHandshakeRoundTrip(t *testing.T) {
	h := newHarness(t, time.Hour, func(s *Server) {
		s.BuildSHA = "abc123"
		s.AdvertiseWireMinor = 4
	})
	c := h.dial()
	hs := c.Handshake()
	if hs.Legacy || hs.WireMajor != WireMajor || hs.WireMinor != 4 || hs.BuildSHA != "abc123" {
		t.Fatalf("handshake %+v", hs)
	}
	if Decide(WireMajor, hs) != ActionAdopt {
		t.Fatalf("decision %v", Decide(WireMajor, hs))
	}
	if !hs.Allows(CapPause) || !hs.Allows(CapHeartbeat) || hs.Allows("not-yet") {
		t.Fatalf("capabilities %+v", hs.Capabilities)
	}
	if _, err := c.StartVM(context.Background(), StartRequest{Spec: spec(), BootID: "boot-1", HMACToken: "token-1", ImageDigest: "sha256:abc"}); err != nil {
		t.Fatal(err)
	}
	again := h.dial()
	if again.Handshake().PodUID != "uid-1" {
		t.Fatalf("pod uid %q", again.Handshake().PodUID)
	}

	// The agent hello may carry a field this supervisor does not know.
	// The server still answers ready.
	conn, err := net.Dial("unix", SocketPath(h.dir, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if _, err := readRaw(conn); err != nil {
		t.Fatal(err)
	}
	agent := []byte(`{"op":"hello","ok":true,"protocol_major":1,"protocol_minor":0,"role":"agent","slot":0,"wire_major":1,"wire_minor":0,"build_sha":"agent","future_field":{"n":1}}`)
	if err := writeRaw(conn, agent); err != nil {
		t.Fatal(err)
	}
	body, err := readRaw(conn)
	if err != nil {
		t.Fatal(err)
	}
	var ready message
	if err := json.Unmarshal(body, &ready); err != nil {
		t.Fatal(err)
	}
	if !ready.OK || ready.Op != opReady {
		t.Fatalf("ready %+v", ready)
	}
}

func TestUnixHandshakeMinorAheadUnknownField(t *testing.T) {
	dir := socketRunDir(t)
	path := SocketPath(dir, 0)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		hello := []byte(`{"op":"hello","ok":true,"protocol_major":1,"protocol_minor":3,"binary_version":"sv-new","role":"supervisor","slot":0,"pid":1,"wire_major":1,"wire_minor":9,"build_sha":"newer","capabilities":["pause","not-yet"],"future_field":{"n":1},"workload_started":true}`)
		if err := writeRaw(conn, hello); err != nil {
			return
		}
		if _, err := readRaw(conn); err != nil {
			return
		}
		_ = writeRaw(conn, []byte(`{"op":"ready","ok":true}`))
		_, _ = readRaw(conn)
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	c, err := Dial(context.Background(), path, "agent-new", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	hs := c.Handshake()
	if hs.WireMinor != 9 || hs.BuildSHA != "newer" || !hs.WorkloadStarted {
		t.Fatalf("handshake %+v", hs)
	}
	if Decide(WireMajor, hs) != ActionAdopt {
		t.Fatalf("decision %v", Decide(WireMajor, hs))
	}
	if hs.Allows("not-yet") || !hs.Allows(CapPause) || MayStartWorkload(hs) {
		t.Fatalf("caps %+v start %v", hs.Capabilities, MayStartWorkload(hs))
	}
}

func TestUnixHandshakeLegacyIsWireMajorZero(t *testing.T) {
	dir := socketRunDir(t)
	path := SocketPath(dir, 0)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		hello := []byte(`{"op":"hello","ok":true,"protocol_major":1,"protocol_minor":0,"binary_version":"sv-old","role":"supervisor","slot":0,"pid":4}`)
		if err := writeRaw(conn, hello); err != nil {
			return
		}
		if _, err := readRaw(conn); err != nil {
			return
		}
		_ = writeRaw(conn, []byte(`{"op":"ready","ok":true}`))
		_, _ = readRaw(conn)
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		<-done
	})
	c, err := Dial(context.Background(), path, "agent-new", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	hs := c.Handshake()
	if !hs.Legacy || hs.WireMajor != 0 || Decide(WireMajor, hs) != ActionHold {
		t.Fatalf("handshake %+v decision %v", hs, Decide(WireMajor, hs))
	}
	if HoldReason(WireMajor, hs) == "" {
		t.Fatal("empty hold reason")
	}
}

func TestUnixNoteWorkloadSurvivesASecondDial(t *testing.T) {
	h := newHarness(t, time.Hour, nil)
	c := h.dial()
	code := int32(9)
	if err := c.NoteWorkload(context.Background(), WorkloadNote{Started: true, Exited: true, ExitCode: &code}); err != nil {
		t.Fatal(err)
	}
	again := h.dial()
	hs := again.Handshake()
	got, ok := WorkloadResult(hs)
	if !hs.WorkloadStarted || !ok || got != 9 || MayStartWorkload(hs) {
		t.Fatalf("handshake %+v result (%d, %v)", hs, got, ok)
	}
	if Decide(WireMajor, hs) != ActionAdopt {
		t.Fatalf("decision %v", Decide(WireMajor, hs))
	}
}

func TestSetCapabilitiesRestrictsPause(t *testing.T) {
	h := newHarness(t, time.Hour, func(s *Server) {
		s.SetCapabilities([]string{CapHeartbeat})
	})
	c := h.dial()
	if c.Handshake().Allows(CapPause) {
		t.Fatal("pause was advertised")
	}
	if err := c.PauseAndSaveVM(context.Background(), os.DevNull); err == nil {
		t.Fatal("pause without the capability succeeded")
	}
}
