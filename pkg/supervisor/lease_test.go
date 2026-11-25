package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestLeaseAtomicWriteAndMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	lease := Lease{
		PodUID: "uid", PodName: "pod", Namespace: "ns",
		BootID: "boot-1", ImageDigest: "sha256:abc",
		SupervisorPID: 7, SupervisorStartTimeNS: 99, HMACToken: "tok",
	}
	if err := WriteLease(dir, 0, lease); err != nil {
		t.Fatal(err)
	}
	lease.BootID = "boot-2"
	if err := WriteLease(dir, 0, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(LeasePath(dir, 0) + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temp lease still present: %v", err)
	}
	got, err := ReadLease(dir, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.BootID != "boot-2" || got.HMACToken != "tok" || got.SupervisorPID != 7 {
		t.Fatalf("lease %+v", got)
	}
	file, err := os.Stat(LeasePath(dir, 0))
	if err != nil {
		t.Fatal(err)
	}
	if file.Mode().Perm() != 0o600 {
		t.Fatalf("lease mode %#o", file.Mode().Perm())
	}
	slotDir := filepath.Dir(LeasePath(dir, 0))
	st, err := os.Stat(slotDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o700 {
		t.Fatalf("slot dir mode %#o", st.Mode().Perm())
	}
	parent, err := os.Stat(filepath.Dir(slotDir))
	if err != nil {
		t.Fatal(err)
	}
	if parent.Mode().Perm() != 0o700 {
		t.Fatalf("slots dir mode %#o", parent.Mode().Perm())
	}
	if err := RemoveLease(dir, 0); err != nil {
		t.Fatal(err)
	}
	if err := RemoveLease(dir, 0); err != nil {
		t.Fatal(err)
	}
}

func TestFence(t *testing.T) {
	ok := Lease{
		PodUID: "uid", PodName: "pod", Namespace: "ns",
		BootID: "boot", ImageDigest: "sha256:abc",
		SupervisorPID: 4, SupervisorStartTimeNS: 8, HMACToken: "abcd",
	}
	insp := Inspect{
		PodUID: "uid", PodName: "pod", Namespace: "ns",
		BootID: "boot", ImageDigest: "sha256:abc",
		PID: 4, StartTimeNS: 8, HMACToken: "abcd",
	}
	pod := PodPresence{Found: true, UID: "uid"}
	if err := Fence(ok, insp, pod); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		edit func(*Lease, *Inspect, *PodPresence)
	}{
		{"boot", func(l *Lease, _ *Inspect, _ *PodPresence) { l.BootID = "other" }},
		{"token", func(l *Lease, _ *Inspect, _ *PodPresence) { l.HMACToken = "zzzz" }},
		{"pid", func(l *Lease, _ *Inspect, _ *PodPresence) { l.SupervisorPID = 5 }},
		{"start", func(l *Lease, _ *Inspect, _ *PodPresence) { l.SupervisorStartTimeNS = 9 }},
		{"digest", func(l *Lease, _ *Inspect, _ *PodPresence) { l.ImageDigest = "sha256:no" }},
		{"pod-uid", func(l *Lease, _ *Inspect, _ *PodPresence) { l.PodUID = "other" }},
		{"gone", func(_ *Lease, _ *Inspect, p *PodPresence) { p.Found = false }},
		{"deleting", func(_ *Lease, _ *Inspect, p *PodPresence) { p.Deleting = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l, in, p := ok, insp, pod
			tc.edit(&l, &in, &p)
			if err := Fence(l, in, p); !errors.Is(err, ErrFence) {
				t.Fatalf("fence %v", err)
			}
		})
	}
}

func TestBootIDAndToken(t *testing.T) {
	id, err := NewBootID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 || id[14] != '4' {
		t.Fatalf("boot id %q", id)
	}
	tok, err := NewToken(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 {
		t.Fatalf("token length %d", len(tok))
	}
	other := []byte(tok)
	if other[0] == 'a' {
		other[0] = 'b'
	} else {
		other[0] = 'a'
	}
	if !tokenEqual(tok, tok) || tokenEqual(tok, string(other)) {
		t.Fatal("token compare")
	}
}
