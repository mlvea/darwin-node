package guest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMixSeedIsSHA512OfEntropyUIDAndContinuousTime(t *testing.T) {
	entropy := bytes.Repeat([]byte{0x11}, AdoptEntropyLen)
	uid := "pod-uid-1"
	const ct uint64 = 0x0102030405060708
	got := MixSeed(entropy, uid, ct)

	h := sha512.New()
	_, _ = h.Write(entropy)
	_, _ = h.Write([]byte(uid))
	var be [8]byte
	binary.BigEndian.PutUint64(be[:], ct)
	_, _ = h.Write(be[:])
	want := h.Sum(nil)
	if !bytes.Equal(got, want) || len(got) != 64 {
		t.Fatalf("seed %x", got)
	}
	if bytes.Equal(got, MixSeed(entropy, uid, ct+1)) {
		t.Fatal("continuous time did not change the seed")
	}
}

func TestReseedProofBindsHostEntropyAndSample(t *testing.T) {
	entropy := bytes.Repeat([]byte{0x22}, AdoptEntropyLen)
	sample := bytes.Repeat([]byte{0x33}, AdoptSampleLen)
	got := ReseedProof(entropy, sample)
	sum := sha256.Sum256(append(append([]byte{}, entropy...), sample...))
	if !bytes.Equal(got, sum[:]) {
		t.Fatalf("proof %x", got)
	}
	if bytes.Equal(got, ReseedProof(entropy, bytes.Repeat([]byte{0x34}, AdoptSampleLen))) {
		t.Fatal("sample did not change the proof")
	}
}

func TestAdoptRequestOmitsSessionToken(t *testing.T) {
	req, err := NewAdoptRequest("uid-1", "pod-a", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["token"]; ok {
		t.Fatalf("adopt request carried a token: %s", raw)
	}
	if len(req.HostEntropy) != AdoptEntropyLen || isZero(req.HostEntropy) {
		t.Fatalf("entropy %d bytes", len(req.HostEntropy))
	}
}

func TestPerformAdoptDoesNotTouchPlatformOnBadInput(t *testing.T) {
	now := time.Now()
	good, err := NewAdoptRequest("uid-1", "pod-a", now)
	if err != nil {
		t.Fatal(err)
	}
	cases := []AdoptReq{
		{},
		{PodUID: "uid", PodHostname: "pod", HostEntropy: []byte{1, 2, 3}, HostWallClockUnixNano: now.UnixNano()},
		{PodUID: "uid", PodHostname: "bad host", HostEntropy: good.HostEntropy, HostWallClockUnixNano: now.UnixNano()},
		{PodUID: "uid\n", PodHostname: "pod", HostEntropy: good.HostEntropy, HostWallClockUnixNano: now.UnixNano()},
		{PodUID: "uid", PodHostname: "pod", HostEntropy: bytes.Repeat([]byte{0}, AdoptEntropyLen), HostWallClockUnixNano: now.UnixNano()},
		{PodUID: "uid", PodHostname: "pod", HostEntropy: good.HostEntropy, HostWallClockUnixNano: 0},
	}
	for i, req := range cases {
		os := &scriptOS{ct: 1, uuid: "00112233-4455-4677-8899-aabbccddeeff", cpus: 2}
		if _, err := PerformAdopt(req, os); err == nil {
			t.Fatalf("case %d accepted %#v", i, req.PodHostname)
		}
		if len(os.calls) != 0 {
			t.Fatalf("case %d touched the platform: %v", i, os.calls)
		}
	}
}

func TestPerformAdoptOrderAndFailClosed(t *testing.T) {
	now := time.Now()
	req, err := NewAdoptRequest("uid-9", "web.example", now)
	if err != nil {
		t.Fatal(err)
	}
	os := &scriptOS{
		ct:   0xabc,
		uuid: "00112233-4455-4677-8899-aabbccddeeff",
		cpus: 3,
	}
	res, err := PerformAdopt(req, os)
	if err != nil {
		t.Fatal(err)
	}
	if !res.ReseedOK {
		t.Fatal("reseed_ok false")
	}
	wantSeed := MixSeed(req.HostEntropy, req.PodUID, os.ct)
	if !bytes.Equal(os.wrote, wantSeed) {
		t.Fatalf("wrote %x", os.wrote)
	}
	wantCalls := []string{"continuous", "write", "entropy", "entropy", "entropy", "clock", "names", "ssh", "mdns", "sample", "uuid", "wall"}
	if strings.Join(os.calls, ",") != strings.Join(wantCalls, ",") {
		t.Fatalf("calls %v", os.calls)
	}
	if os.names != "web.example" {
		t.Fatalf("names %q", os.names)
	}
	sample, err := CheckAdopt(req, res, now.Add(10*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	if len(sample) != AdoptSampleLen || isZero(sample) {
		t.Fatalf("sample %x", sample)
	}
	proof := ReseedProof(req.HostEntropy, sample)
	if hex.EncodeToString(proof) != res.ReseedProofSHA256 {
		t.Fatalf("proof %s", res.ReseedProofSHA256)
	}

	os.fail = "ssh"
	os.calls = nil
	res, err = PerformAdopt(req, os)
	if err == nil || res.ReseedOK {
		t.Fatalf("ssh failure returned ok=%v err=%v", res.ReseedOK, err)
	}
	if strings.Contains(strings.Join(os.calls, ","), "mdns") {
		t.Fatalf("continued after ssh failure: %v", os.calls)
	}
}

func TestPerformAdoptRejectsZeroSample(t *testing.T) {
	req, err := NewAdoptRequest("uid", "pod", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	os := &scriptOS{
		ct:     1,
		uuid:   "00112233-4455-4677-8899-aabbccddeeff",
		cpus:   1,
		sample: bytes.Repeat([]byte{0}, AdoptSampleLen),
	}
	res, err := PerformAdopt(req, os)
	if err == nil || res.ReseedOK {
		t.Fatal("all-zero sample was accepted")
	}
}

func TestCheckAdoptRejectsHostileResponses(t *testing.T) {
	now := time.Now()
	req, err := NewAdoptRequest("uid", "pod", now)
	if err != nil {
		t.Fatal(err)
	}
	sample := bytes.Repeat([]byte{0x5a}, AdoptSampleLen)
	good, err := AcceptedAdopt(req, sample, "00112233-4455-4677-8899-aabbccddeeff")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckAdopt(req, good, now); err != nil {
		t.Fatal(err)
	}
	bad := good
	bad.ReseedOK = false
	if _, err := CheckAdopt(req, bad, now); err == nil {
		t.Fatal("reseed_ok false accepted")
	}
	bad = good
	bad.GuestRNGSampleHex = hex.EncodeToString(bytes.Repeat([]byte{0}, AdoptSampleLen))
	bad.ReseedProofSHA256 = hex.EncodeToString(ReseedProof(req.HostEntropy, bytes.Repeat([]byte{0}, AdoptSampleLen)))
	if _, err := CheckAdopt(req, bad, now); err == nil {
		t.Fatal("all-zero sample accepted")
	}
	bad = good
	bad.ReseedProofSHA256 = hex.EncodeToString(bytes.Repeat([]byte{1}, 32))
	if _, err := CheckAdopt(req, bad, now); err == nil {
		t.Fatal("wrong proof accepted")
	}
	bad = good
	bad.GuestRNGSampleHex = ""
	if _, err := CheckAdopt(req, bad, now); err == nil {
		t.Fatal("empty sample accepted")
	}
	bad = good
	bad.ClockUnixNano = now.Add(-2 * time.Second).UnixNano()
	if _, err := CheckAdopt(req, bad, now); err == nil {
		t.Fatal("skewed clock accepted")
	}
	bad = good
	bad.IOPlatformUUID = "not a uuid"
	if _, err := CheckAdopt(req, bad, now); err == nil {
		t.Fatal("bad platform uuid accepted")
	}
}

func TestAdoptRoundTripIgnoresTokenField(t *testing.T) {
	h := Handler{Token: "secret", AgentVersion: "test", AdoptPlatform: &scriptOS{
		ct: 7, uuid: "00112233-4455-4677-8899-aabbccddeeff", cpus: 2,
	}}
	h.Init()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() { _ = Serve(ctx, a, h) }()
	cli, err := Dial(ctx, b, "secret", "host")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if !cli.HasCapability(CapAdopt) {
		t.Fatal("agent did not advertise adopt")
	}
	req, err := NewAdoptRequest("uid-rt", "pod-rt", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// A hostile or confused host must not need the session token in this RPC.
	raw, _ := json.Marshal(req)
	var withToken map[string]any
	_ = json.Unmarshal(raw, &withToken)
	withToken["token"] = "secret"
	body, _ := json.Marshal(withToken)
	env, err := cli.sess.Call(ctx, MethodAdopt, json.RawMessage(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := Decode[AdoptRes](env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckAdopt(req, res, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveSSHHostKeysOnlyDeletesHostKeys(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"ssh_host_ed25519_key", "ssh_host_rsa_key.pub", "sshd_config", "ssh_host_dir"} {
		path := filepath.Join(dir, name)
		if name == "ssh_host_dir" {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(path, []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := removeSSHHostKeys(dir); err == nil {
		t.Fatal("a directory named ssh_host_* must fail closed")
	}
	if err := os.Remove(filepath.Join(dir, "ssh_host_dir")); err != nil {
		t.Fatal(err)
	}
	if err := removeSSHHostKeys(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sshd_config")); err != nil {
		t.Fatal(err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "ssh_host_*"))
	if len(matches) != 0 {
		t.Fatalf("keys left: %v", matches)
	}
	if err := removeSSHHostKeys(dir); err != nil {
		t.Fatal(err)
	}
}

func TestSoftwarePlatformRequested(t *testing.T) {
	t.Setenv(EnvGuestSoftwarePlatform, "")
	if softwarePlatformRequested() {
		t.Fatal("empty env selected the software platform")
	}
	t.Setenv(EnvGuestSoftwarePlatform, "1")
	if !softwarePlatformRequested() {
		t.Fatal("1 did not select the software platform")
	}
	t.Setenv(EnvGuestSoftwarePlatform, "true")
	if !softwarePlatformRequested() {
		t.Fatal("true did not select the software platform")
	}
}

func TestParseIOPlatformUUID(t *testing.T) {
	const sample = `"IOPlatformUUID" = "00112233-4455-4677-8899-AABBCCDDEEFF"`
	got, err := parseIOPlatformUUID(sample)
	if err != nil || got != "00112233-4455-4677-8899-AABBCCDDEEFF" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := parseIOPlatformUUID("no uuid here"); err == nil {
		t.Fatal("missing uuid accepted")
	}
}

func TestLocalHostLabel(t *testing.T) {
	got, err := localHostLabel("web.example")
	if err != nil || got != "web-example" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := localHostLabel(""); err == nil {
		t.Fatal("empty label accepted")
	}
}

// scriptOS is the Linux stand-in for the guest OS steps.
type scriptOS struct {
	mu     sync.Mutex
	calls  []string
	ct     uint64
	fail   string
	sample []byte
	clock  int64
	uuid   string
	cpus   int
	names  string
	wrote  []byte
}

func (s *scriptOS) note(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, name)
	if s.fail == name {
		return errors.New(name)
	}
	return nil
}

func (s *scriptOS) ContinuousTime() (uint64, error) {
	if err := s.note("continuous"); err != nil {
		return 0, err
	}
	return s.ct, nil
}

func (s *scriptOS) WriteRandom(seed []byte) error {
	if err := s.note("write"); err != nil {
		return err
	}
	if len(seed) != 64 {
		return errors.New("seed length")
	}
	s.mu.Lock()
	s.wrote = append([]byte(nil), seed...)
	s.mu.Unlock()
	return nil
}

func (s *scriptOS) Entropy(n int) ([]byte, error) {
	name := "entropy"
	if n == AdoptSampleLen {
		name = "sample"
	}
	if err := s.note(name); err != nil {
		return nil, err
	}
	if n == AdoptSampleLen && len(s.sample) == n {
		return append([]byte(nil), s.sample...), nil
	}
	out := bytes.Repeat([]byte{0x42}, n)
	out[0] = byte(n)
	return out, nil
}

func (s *scriptOS) SetWallClock(unixNano int64) error {
	if err := s.note("clock"); err != nil {
		return err
	}
	s.mu.Lock()
	s.clock = unixNano
	s.mu.Unlock()
	return nil
}

func (s *scriptOS) SetHostNames(hostname string) error {
	if err := s.note("names"); err != nil {
		return err
	}
	s.mu.Lock()
	s.names = hostname
	s.mu.Unlock()
	return nil
}

func (s *scriptOS) DeleteSSHHostKeys() error { return s.note("ssh") }

func (s *scriptOS) SignalMDNSResponder() error { return s.note("mdns") }

func (s *scriptOS) PlatformUUID() (string, error) {
	if err := s.note("uuid"); err != nil {
		return "", err
	}
	return s.uuid, nil
}

func (s *scriptOS) WallClock() (int64, error) {
	if err := s.note("wall"); err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clock == 0 {
		return 0, errors.New("clock unset")
	}
	return s.clock, nil
}

func (s *scriptOS) NumCPU() int {
	if s.cpus < 1 {
		return 1
	}
	return s.cpus
}
