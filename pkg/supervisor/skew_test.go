package supervisor

import (
	"encoding/json"
	"testing"
)

// TestDecideVersionSkew is the wire contract from Q13.
// Equal majors re-adopt. A different build sha, a minor that is ahead
// or behind, and unknown JSON do not matter. A different major, an
// explicit 0, or a supervisor that predates the handshake (no version
// fields) is a hold: the VM stays up and the slot stays occupied.
func TestDecideVersionSkew(t *testing.T) {
	code := int32(7)
	cases := []struct {
		name        string
		raw         string
		want        Action
		legacy      bool
		allowPause  bool
		allowFuture bool
		startWork   bool
		exitCode    int32
		exitOK      bool
	}{
		{
			name:       "equal major different sha",
			raw:        `{"wire_major":1,"wire_minor":0,"build_sha":"deadbeef","capabilities":["heartbeat","pause"],"slot":0,"pod_uid":"uid-1","host_boot_id":"boot","workload_started":false}`,
			want:       ActionAdopt,
			allowPause: true,
			startWork:  true,
		},
		{
			name:        "minor ahead unknown field and capability",
			raw:         `{"wire_major":1,"wire_minor":9,"build_sha":"newer","capabilities":["pause","not-yet"],"slot":1,"pod_uid":"uid-2","future_field":{"n":1},"workload_started":true}`,
			want:        ActionAdopt,
			allowPause:  true,
			allowFuture: false,
			startWork:   false,
		},
		{
			name:       "minor behind no optional pause",
			raw:        `{"wire_major":1,"wire_minor":0,"build_sha":"older","capabilities":["heartbeat"],"slot":0,"pod_uid":"uid-3","agent_extension":true}`,
			want:       ActionAdopt,
			allowPause: false,
			startWork:  true,
		},
		{
			name:       "major mismatch",
			raw:        `{"wire_major":2,"wire_minor":0,"build_sha":"other","capabilities":["pause"],"slot":0,"pod_uid":"uid-4"}`,
			want:       ActionHold,
			allowPause: true,
			startWork:  true,
		},
		{
			name:      "explicit wire major zero",
			raw:       `{"wire_major":0,"wire_minor":4,"build_sha":"zero","slot":0}`,
			want:      ActionHold,
			startWork: true,
		},
		{
			name:      "legacy supervisor has no version fields",
			raw:       `{"op":"hello","ok":true,"protocol_major":1,"protocol_minor":0,"binary_version":"sv-old","role":"supervisor","slot":0,"pid":4}`,
			want:      ActionHold,
			legacy:    true,
			startWork: true,
		},
		{
			name:       "exited workload reports code and does not start again",
			raw:        `{"wire_major":1,"wire_minor":1,"build_sha":"same","capabilities":["pause"],"slot":0,"pod_uid":"uid-5","workload_started":true,"workload_exited":true,"exit_code":7}`,
			want:       ActionAdopt,
			allowPause: true,
			exitCode:   code,
			exitOK:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hs, err := ParseHandshake([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if got := Decide(WireMajor, hs); got != tc.want {
				t.Fatalf("Decide = %v, want %v (handshake %+v)", got, tc.want, hs)
			}
			if hs.Legacy != tc.legacy {
				t.Fatalf("legacy %v, want %v", hs.Legacy, tc.legacy)
			}
			if hs.Allows(CapPause) != tc.allowPause {
				t.Fatalf("pause allowed %v, want %v (caps %v)", hs.Allows(CapPause), tc.allowPause, hs.Capabilities)
			}
			if hs.Allows("not-yet") != tc.allowFuture {
				t.Fatalf("unknown capability allowed %v", hs.Allows("not-yet"))
			}
			if MayStartWorkload(hs) != tc.startWork {
				t.Fatalf("MayStartWorkload %v, want %v", MayStartWorkload(hs), tc.startWork)
			}
			gotCode, ok := WorkloadResult(hs)
			if ok != tc.exitOK || (ok && gotCode != tc.exitCode) {
				t.Fatalf("workload result (%d, %v), want (%d, %v)", gotCode, ok, tc.exitCode, tc.exitOK)
			}
			if tc.want == ActionHold {
				reason := HoldReason(WireMajor, hs)
				if reason == "" {
					t.Fatal("hold reason is empty")
				}
			}
		})
	}
}

// TestHandshakeMinorTracksFieldSet fails when a field is added to the
// handshake without a wire_minor bump. Unknown fields are not part of
// this set; peers ignore them.
func TestHandshakeMinorTracksFieldSet(t *testing.T) {
	if WireMinor != 0 {
		t.Fatalf("wire minor %d: update the expected key set in this test in the same change", WireMinor)
	}
	code := int32(3)
	body, err := MarshalHandshake(Handshake{
		WireMajor:       WireMajor,
		WireMinor:       WireMinor,
		BuildSHA:        "abc",
		Capabilities:    []string{"pause"},
		Slot:            1,
		PodUID:          "uid",
		HostBootID:      "boot",
		WorkloadStarted: true,
		WorkloadExited:  true,
		ExitCode:        &code,
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(body, &keys); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"wire_major", "wire_minor", "build_sha", "capabilities", "slot",
		"pod_uid", "host_boot_id", "workload_started", "workload_exited", "exit_code",
	}
	if len(keys) != len(want) {
		t.Fatalf("handshake keys %d, want %d: %s", len(keys), len(want), body)
	}
	for _, k := range want {
		if _, ok := keys[k]; !ok {
			t.Fatalf("missing %s in %s", k, body)
		}
	}
	// A peer that adds a field must still parse. The extra key is ignored.
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(body, &generic); err != nil {
		t.Fatal(err)
	}
	generic["future_field"] = json.RawMessage(`{"n":1}`)
	bumped, err := json.Marshal(generic)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := ParseHandshake(bumped)
	if err != nil {
		t.Fatal(err)
	}
	if hs.WireMajor != WireMajor || hs.BuildSHA != "abc" || hs.Slot != 1 || hs.PodUID != "uid" {
		t.Fatalf("parsed %+v", hs)
	}
	if !hs.WorkloadStarted || !hs.WorkloadExited || hs.ExitCode == nil || *hs.ExitCode != 3 {
		t.Fatalf("workload %+v", hs)
	}
	if Decide(WireMajor, hs) != ActionAdopt {
		t.Fatalf("extra field changed the decision: %+v", hs)
	}
}
