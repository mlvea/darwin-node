package guest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	// AdoptEntropyLen is the host CSPRNG input. It is fresh for every pod
	// boot and is not the agent session token.
	AdoptEntropyLen = 64
	// AdoptSampleLen is the guest sample the host stores and compares.
	AdoptSampleLen = 32
	// AdoptClockSkew is how far the guest clock may sit from the host at
	// the moment the response is checked. The guest is hostile, so a
	// response that did not apply the host time is a failed reseed.
	AdoptClockSkew = 500 * time.Millisecond

	// EnvGuestSoftwarePlatform selects the in-memory platform. SimVZ sets
	// it on a darwin-guest-agent child so a test process on the Mac that
	// is running the tests does not reseed that Mac or rename it. The
	// launchd job inside a guest image does not set it. Default off.
	EnvGuestSoftwarePlatform = "DARWIN_NODE_GUEST_SOFTWARE_PLATFORM"
)

func softwarePlatformRequested() bool {
	v := strings.TrimSpace(os.Getenv(EnvGuestSoftwarePlatform))
	return v == "1" || strings.EqualFold(v, "true")
}

// AdoptReq is the host→guest reseed barrier. It is sent after the
// handshake and before Materialize or Workload. The session token is not
// a field: encoding/json will not emit one, and a token stuffed into the
// object is ignored.
type AdoptReq struct {
	PodUID                string `json:"pod_uid"`
	PodHostname           string `json:"pod_hostname"`
	HostEntropy           []byte `json:"host_entropy_64b"`
	HostWallClockUnixNano int64  `json:"host_wall_clock_unix_nano"`
}

// AdoptRes is the guest's proof that the reseed ran. guest_rng_sample_hex
// is 32 bytes, hex encoded. reseed_proof_sha256 is SHA256(host_entropy_64b
// || those 32 bytes), hex encoded. The host recomputes the proof from the
// entropy it sent. It does not trust a proof the guest computed over some
// other input.
type AdoptRes struct {
	ReseedOK          bool   `json:"reseed_ok"`
	GuestRNGSampleHex string `json:"guest_rng_sample_hex"`
	ReseedProofSHA256 string `json:"reseed_proof_sha256"`
	IOPlatformUUID    string `json:"io_platform_uuid"`
	ClockUnixNano     int64  `json:"clock_unix_nano"`
}

// AdoptPlatform is the OS the reseed touches. Linux tests and SimVZ pass
// a fake. The darwin guest agent passes the real one. A nil platform is
// not used: PerformAdopt refuses it.
type AdoptPlatform interface {
	ContinuousTime() (uint64, error)
	WriteRandom(seed []byte) error
	// Entropy reads n bytes. PerformAdopt calls it on a thread locked to
	// one CPU: 64 bytes on each CPU to refresh that CPU's generator, then
	// 32 bytes for the sample the host checks.
	Entropy(n int) ([]byte, error)
	SetWallClock(unixNano int64) error
	SetHostNames(hostname string) error
	DeleteSSHHostKeys() error
	SignalMDNSResponder() error
	PlatformUUID() (string, error)
	WallClock() (int64, error)
	NumCPU() int
}

var (
	hostnameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
	uuidRE     = regexp.MustCompile(`^[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}$`)
)

func adoptClockMin() int64 {
	return time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
}

func adoptClockMax() int64 {
	return time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano()
}

// ValidateAdoptRequest rejects a request the guest must not apply.
// Validation finishes before any OS call.
func ValidateAdoptRequest(req AdoptReq) error {
	if err := validatePodUID(req.PodUID); err != nil {
		return err
	}
	if err := ValidateHostname(req.PodHostname); err != nil {
		return err
	}
	if len(req.HostEntropy) != AdoptEntropyLen || isZero(req.HostEntropy) {
		return errors.New("host entropy must be 64 non-zero bytes")
	}
	if req.HostWallClockUnixNano < adoptClockMin() || req.HostWallClockUnixNano >= adoptClockMax() {
		return errors.New("host wall clock is outside 2001..2100")
	}
	return nil
}

func validatePodUID(uid string) error {
	if uid == "" || len(uid) > 1024 {
		return errors.New("pod uid length")
	}
	for _, r := range uid {
		if r < 0x20 || r == 0x7f {
			return errors.New("pod uid has a control character")
		}
	}
	return nil
}

// ValidateHostname is the name scutil will receive. It is a DNS hostname,
// not a shell string: the darwin platform passes it as an argument.
func ValidateHostname(host string) error {
	if host == "" || len(host) > 253 || !hostnameRE.MatchString(host) {
		return fmt.Errorf("hostname %q", host)
	}
	return nil
}

func validatePlatformUUID(id string) error {
	if !uuidRE.MatchString(id) {
		return errors.New("adopt: io platform uuid")
	}
	return nil
}

// parseIOPlatformUUID reads the IOPlatformUUID line from ioreg -rd1 output.
func parseIOPlatformUUID(out string) (string, error) {
	const key = `"IOPlatformUUID"`
	i := strings.Index(out, key)
	if i < 0 {
		return "", errors.New("IOPlatformUUID missing")
	}
	rest := out[i+len(key):]
	q1 := strings.Index(rest, `"`)
	if q1 < 0 {
		return "", errors.New("IOPlatformUUID missing")
	}
	rest = rest[q1+1:]
	q2 := strings.Index(rest, `"`)
	if q2 < 0 {
		return "", errors.New("IOPlatformUUID missing")
	}
	id := rest[:q2]
	if err := validatePlatformUUID(id); err != nil {
		return "", err
	}
	return id, nil
}

// MixSeed is SHA512(host_entropy_64b || pod_uid || mach_continuous_time).
// mach_continuous_time is 8 bytes, big endian. The result is 64 bytes,
// which is what XNU write_random consumes.
func MixSeed(entropy []byte, podUID string, continuous uint64) []byte {
	h := sha512.New()
	_, _ = h.Write(entropy)
	_, _ = h.Write([]byte(podUID))
	var be [8]byte
	binary.BigEndian.PutUint64(be[:], continuous)
	_, _ = h.Write(be[:])
	return h.Sum(nil)
}

// ReseedProof is SHA256(host_entropy_64b || guest_sample). guest_sample is
// the raw 32 bytes, not the hex text.
func ReseedProof(entropy, sample []byte) []byte {
	buf := make([]byte, 0, len(entropy)+len(sample))
	buf = append(buf, entropy...)
	buf = append(buf, sample...)
	sum := sha256.Sum256(buf)
	return sum[:]
}

// NewAdoptRequest draws 64 bytes from crypto/rand and stamps the host clock.
// The session token is not an argument.
func NewAdoptRequest(podUID, hostname string, now time.Time) (AdoptReq, error) {
	var entropy [AdoptEntropyLen]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return AdoptReq{}, fmt.Errorf("adopt entropy: %w", err)
	}
	req := AdoptReq{
		PodUID:                podUID,
		PodHostname:           hostname,
		HostEntropy:           entropy[:],
		HostWallClockUnixNano: now.UnixNano(),
	}
	if err := ValidateAdoptRequest(req); err != nil {
		return AdoptReq{}, err
	}
	return req, nil
}

// AcceptedAdopt builds a response whose proof matches sample. SimVZ uses it
// to inject an all-zero or repeated sample that still carries a valid proof,
// so the host's own checks are what reject it.
func AcceptedAdopt(req AdoptReq, sample []byte, uuid string) (AdoptRes, error) {
	if len(sample) != AdoptSampleLen {
		return AdoptRes{}, errors.New("adopt sample length")
	}
	if err := validatePlatformUUID(uuid); err != nil {
		return AdoptRes{}, err
	}
	return AdoptRes{
		ReseedOK:          true,
		GuestRNGSampleHex: hex.EncodeToString(sample),
		ReseedProofSHA256: hex.EncodeToString(ReseedProof(req.HostEntropy, sample)),
		IOPlatformUUID:    uuid,
		ClockUnixNano:     req.HostWallClockUnixNano,
	}, nil
}

// PerformAdopt runs the reseed and returns only after every step succeeded.
// A platform error returns reseed_ok false and does not invent a proof.
func PerformAdopt(req AdoptReq, p AdoptPlatform) (AdoptRes, error) {
	if err := ValidateAdoptRequest(req); err != nil {
		return AdoptRes{}, err
	}
	if p == nil {
		return AdoptRes{}, errors.New("adopt platform is nil")
	}
	ct, err := p.ContinuousTime()
	if err != nil {
		return AdoptRes{}, err
	}
	seed := MixSeed(req.HostEntropy, req.PodUID, ct)
	if err := p.WriteRandom(seed); err != nil {
		return AdoptRes{}, err
	}
	if err := refreshCPUs(p); err != nil {
		return AdoptRes{}, err
	}
	if err := p.SetWallClock(req.HostWallClockUnixNano); err != nil {
		return AdoptRes{}, err
	}
	if err := p.SetHostNames(req.PodHostname); err != nil {
		return AdoptRes{}, err
	}
	if err := p.DeleteSSHHostKeys(); err != nil {
		return AdoptRes{}, err
	}
	if err := p.SignalMDNSResponder(); err != nil {
		return AdoptRes{}, err
	}
	sample, err := readSample(p)
	if err != nil {
		return AdoptRes{}, err
	}
	if len(sample) != AdoptSampleLen || isZero(sample) {
		return AdoptRes{}, errors.New("guest rng sample is zero")
	}
	uuid, err := p.PlatformUUID()
	if err != nil {
		return AdoptRes{}, err
	}
	if err := validatePlatformUUID(uuid); err != nil {
		return AdoptRes{}, err
	}
	clock, err := p.WallClock()
	if err != nil {
		return AdoptRes{}, err
	}
	return AdoptRes{
		ReseedOK:          true,
		GuestRNGSampleHex: hex.EncodeToString(sample),
		ReseedProofSHA256: hex.EncodeToString(ReseedProof(req.HostEntropy, sample)),
		IOPlatformUUID:    uuid,
		ClockUnixNano:     clock,
	}, nil
}

func refreshCPUs(p AdoptPlatform) error {
	n := p.NumCPU()
	if n < 1 {
		n = 1
	}
	if n > 256 {
		n = 256
	}
	errc := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			_, err := p.Entropy(64)
			errc <- err
		}()
	}
	wg.Wait()
	close(errc)
	for err := range errc {
		if err != nil {
			return err
		}
	}
	return nil
}

func readSample(p AdoptPlatform) ([]byte, error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	return p.Entropy(AdoptSampleLen)
}

// CheckAdopt is the host's view of a response. sample is the raw 32 bytes
// when the proof, the clock, and the platform UUID all check out. The
// node-wide uniqueness set is the caller's; this function does not see it.
func CheckAdopt(req AdoptReq, res AdoptRes, now time.Time) ([]byte, error) {
	if err := ValidateAdoptRequest(req); err != nil {
		return nil, err
	}
	if !res.ReseedOK {
		return nil, errors.New("adopt: guest reseed failed")
	}
	sample, err := hex.DecodeString(res.GuestRNGSampleHex)
	if err != nil || len(sample) == 0 {
		return nil, errors.New("adopt: guest rng sample missing")
	}
	if len(sample) != AdoptSampleLen {
		return nil, errors.New("adopt: guest rng sample length")
	}
	if isZero(sample) {
		return nil, errors.New("adopt: guest rng sample is zero")
	}
	proof, err := hex.DecodeString(res.ReseedProofSHA256)
	want := ReseedProof(req.HostEntropy, sample)
	if err != nil || len(proof) != len(want) || subtle.ConstantTimeCompare(proof, want) != 1 {
		return nil, errors.New("adopt: reseed proof mismatch")
	}
	delta := res.ClockUnixNano - now.UnixNano()
	if delta < 0 {
		delta = -delta
	}
	if res.ClockUnixNano == 0 || delta > AdoptClockSkew.Nanoseconds() {
		return nil, errors.New("adopt: guest clock differs from the host")
	}
	if err := validatePlatformUUID(res.IOPlatformUUID); err != nil {
		return nil, err
	}
	return append([]byte(nil), sample...), nil
}

func isZero(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	var acc byte
	for _, c := range b {
		acc |= c
	}
	return acc == 0
}

// localHostLabel is the Bonjour LocalHostName: one label, letters, digits,
// and hyphens, at most 63 bytes. Dots in the pod hostname become hyphens.
func localHostLabel(host string) (string, error) {
	var b strings.Builder
	prevHyphen := false
	for _, r := range host {
		var c byte
		switch {
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			c = byte(r)
			prevHyphen = false
		case r == '.', r == '-', r == '_':
			if b.Len() == 0 || prevHyphen {
				continue
			}
			c = '-'
			prevHyphen = true
		default:
			continue
		}
		if b.Len() >= 63 {
			break
		}
		b.WriteByte(c)
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "", errors.New("hostname has no local name")
	}
	return s, nil
}

// removeSSHHostKeys deletes ssh_host_* inside dir. A directory with that
// prefix is refused. sshd_config and anything else is left in place. An
// empty match is success: the post-condition is that no host key is there.
func removeSSHHostKeys(dir string) error {
	matches, err := filepath.Glob(filepath.Join(dir, "ssh_host_*"))
	if err != nil {
		return err
	}
	for _, path := range matches {
		fi, err := os.Lstat(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if fi.IsDir() {
			return fmt.Errorf("ssh host key path %s is a directory", path)
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (h *Handler) adopt(ctx context.Context, req AdoptReq) (AdoptRes, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return AdoptRes{}, err
		}
	}
	if h != nil && h.AdoptFn != nil {
		return h.AdoptFn(ctx, req)
	}
	var p AdoptPlatform
	if h != nil {
		p = h.AdoptPlatform
	}
	if p == nil {
		p = NewSoftwarePlatform()
	}
	return PerformAdopt(req, p)
}

// Adopt asks the guest to reseed before it is marked Ready. The caller
// checks the response with CheckAdopt. This method does not.
func (c *Client) Adopt(ctx context.Context, req AdoptReq) (AdoptRes, error) {
	if c == nil || c.sess == nil {
		return AdoptRes{}, errors.New("adopt: no session")
	}
	env, err := c.sess.Call(ctx, MethodAdopt, req)
	if err != nil {
		return AdoptRes{}, err
	}
	return Decode[AdoptRes](env)
}

// SoftwarePlatform performs the reseed in memory. It does not write
// /dev/random, set the clock, run scutil, or delete host keys. SimVZ and
// the Linux guest agent use it. The darwin guest agent does not.
type SoftwarePlatform struct {
	mu    sync.Mutex
	clock int64
	uuid  string
}

// NewSoftwarePlatform returns a platform with no OS side effects.
func NewSoftwarePlatform() *SoftwarePlatform {
	return &SoftwarePlatform{}
}

var softwareBoot = time.Now()

func (p *SoftwarePlatform) ContinuousTime() (uint64, error) {
	d := time.Since(softwareBoot)
	if d < 0 {
		d = 0
	}
	return uint64(d), nil
}

func (p *SoftwarePlatform) WriteRandom(seed []byte) error {
	if len(seed) != 64 {
		return fmt.Errorf("devrandom seed is %d bytes", len(seed))
	}
	return nil
}

func (p *SoftwarePlatform) Entropy(n int) ([]byte, error) {
	if n <= 0 || n > 256 {
		return nil, fmt.Errorf("entropy length %d", n)
	}
	out := make([]byte, n)
	if _, err := rand.Read(out); err != nil {
		return nil, err
	}
	if isZero(out) {
		return nil, errors.New("entropy read is zero")
	}
	return out, nil
}

func (p *SoftwarePlatform) SetWallClock(unixNano int64) error {
	if p == nil {
		return errors.New("nil platform")
	}
	p.mu.Lock()
	p.clock = unixNano
	p.mu.Unlock()
	return nil
}

func (p *SoftwarePlatform) SetHostNames(hostname string) error {
	return ValidateHostname(hostname)
}

func (p *SoftwarePlatform) DeleteSSHHostKeys() error { return nil }

func (p *SoftwarePlatform) SignalMDNSResponder() error { return nil }

func (p *SoftwarePlatform) PlatformUUID() (string, error) {
	if p == nil {
		return "", errors.New("nil platform")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.uuid != "" {
		return p.uuid, nil
	}
	id, err := newPlatformUUID()
	if err != nil {
		return "", err
	}
	p.uuid = id
	return id, nil
}

func (p *SoftwarePlatform) WallClock() (int64, error) {
	if p == nil {
		return 0, errors.New("nil platform")
	}
	p.mu.Lock()
	set := p.clock != 0
	p.mu.Unlock()
	if !set {
		return 0, errors.New("clock unset")
	}
	// The stand-in cannot change the machine clock. Reporting the clock
	// it actually has keeps the host's 500ms check on the real reading.
	return time.Now().UnixNano(), nil
}

func (p *SoftwarePlatform) NumCPU() int { return runtime.NumCPU() }

func newPlatformUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
