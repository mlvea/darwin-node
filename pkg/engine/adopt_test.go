package engine

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

const adoptScriptUUID = "00112233-4455-4677-8899-aabbccddeeff"

func TestAdoptSampleSetEvictsOldest(t *testing.T) {
	s := newAdoptSamples(2)
	a := bytes.Repeat([]byte{1}, guest.AdoptSampleLen)
	b := bytes.Repeat([]byte{2}, guest.AdoptSampleLen)
	c := bytes.Repeat([]byte{3}, guest.AdoptSampleLen)
	if err := s.Add(a); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(b); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(a); err == nil {
		t.Fatal("sample still in the set was accepted again")
	}
	if err := s.Add(c); err != nil {
		t.Fatal(err)
	}
	if err := s.Add(a); err != nil {
		t.Fatalf("evicted sample must be reusable: %v", err)
	}
	if err := s.Add(bytes.Repeat([]byte{0}, guest.AdoptSampleLen)); err == nil {
		t.Fatal("all-zero sample accepted")
	}
}

func TestAdoptBeforeMaterializeAndWorkload(t *testing.T) {
	script := &adoptScript{mode: "ok"}
	e := materializeEngine(t, newAdoptRuntime(script))
	pod := podWithVolume("ord", "uid-ord")
	pod.Spec.Containers[0].Command = []string{"/bin/true"}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "ord", corev1.PodRunning)
	if p.Status.Phase != corev1.PodRunning {
		t.Fatalf("phase %s %s", p.Status.Phase, p.Status.Message)
	}
	methods := script.snapshot()
	ready := indexOf(methods, guest.MethodReady)
	adopt := indexOf(methods, guest.MethodAdopt)
	mat := indexOf(methods, guest.MethodMaterialize)
	work := indexOf(methods, guest.MethodWorkload)
	if ready < 0 || adopt < 0 || mat < 0 || work < 0 {
		t.Fatalf("methods %v", methods)
	}
	if ready >= adopt || adopt >= mat || mat >= work {
		t.Fatalf("order %v", methods)
	}
	_ = e.Delete(context.Background(), "default", "ord", 0)
}

func TestAdoptFailClosed(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		assertAdoptStartFailed(t, "error", "injected adopt failure")
	})
	t.Run("zero", func(t *testing.T) {
		assertAdoptStartFailed(t, "zero", "zero")
	})
	t.Run("duplicate", func(t *testing.T) {
		script := &adoptScript{mode: "dup"}
		e := materializeEngine(t, newAdoptRuntime(script))
		if err := e.Create(context.Background(), samplePod("first", "uid-first"), Credentials{}); err != nil {
			t.Fatal(err)
		}
		waitPhase(t, e, "default", "first", corev1.PodRunning)
		second := samplePod("second", "uid-second")
		second.Spec.Containers[0].Command = []string{"/bin/true"}
		if err := e.Create(context.Background(), second, Credentials{}); err != nil {
			t.Fatal(err)
		}
		p := waitPhaseSettled(t, e, "second")
		if p.Status.Phase != corev1.PodFailed || !strings.Contains(p.Status.Message, "reused") {
			t.Fatalf("phase %s msg %q", p.Status.Phase, p.Status.Message)
		}
		if script.workloadCount() != 0 {
			t.Fatalf("workload calls %d", script.workloadCount())
		}
		waitPodStopped(t, e, "second")
		if _, err := e.Get("default", "first"); err != nil {
			t.Fatal(err)
		}
		_ = e.Delete(context.Background(), "default", "first", 0)
	})
}

func TestLegacyGuestRefusedUnlessOptIn(t *testing.T) {
	t.Run("missing capability", func(t *testing.T) {
		assertAdoptStartFailed(t, "nocap", "does not implement Adopt")
	})
	t.Run("unknown method", func(t *testing.T) {
		assertAdoptStartFailed(t, "unknown", "adopt")
	})
	t.Run("opt in", func(t *testing.T) {
		t.Setenv(envAllowLegacyGuest, "1")
		script := &adoptScript{mode: "nocap"}
		e := materializeEngine(t, newAdoptRuntime(script))
		if err := e.Create(context.Background(), samplePod("old", "uid-old"), Credentials{}); err != nil {
			t.Fatal(err)
		}
		waitPhase(t, e, "default", "old", corev1.PodRunning)
		if script.workloadCount() != 0 {
			t.Fatalf("workload calls %d", script.workloadCount())
		}
		_ = e.Delete(context.Background(), "default", "old", 0)
	})
}

func assertAdoptStartFailed(t *testing.T, mode, want string) {
	t.Helper()
	script := &adoptScript{mode: mode}
	e := materializeEngine(t, newAdoptRuntime(script))
	pod := samplePod("bad", "uid-bad")
	pod.Spec.Containers[0].Command = []string{"/bin/true"}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhaseSettled(t, e, "bad")
	if p.Status.Phase != corev1.PodFailed || !strings.Contains(p.Status.Message, want) {
		t.Fatalf("phase %s msg %q, want %q", p.Status.Phase, p.Status.Message, want)
	}
	if script.workloadCount() != 0 {
		t.Fatalf("workload calls %d after %s", script.workloadCount(), mode)
	}
	waitVMReaped(t, e, script.inner, "bad", "uid-bad")
}

func waitPodStopped(t *testing.T, e *Engine, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.mu.RLock()
		rec := e.pods[Key("default", name)]
		e.mu.RUnlock()
		if rec != nil {
			rec.mu.Lock()
			phase := rec.phase
			var st types.VMState
			if rec.machine != nil {
				st = rec.machine.Status().State
			}
			rec.mu.Unlock()
			if phase == corev1.PodFailed && st == types.VMStopped {
				return
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("%s did not stop", name)
}

type adoptScript struct {
	mu        sync.Mutex
	methods   []string
	workloads int
	mode      string
	inner     *fake.Runtime
}

func (s *adoptScript) note(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods = append(s.methods, method)
	if method == guest.MethodWorkload {
		s.workloads++
	}
}

func (s *adoptScript) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.methods...)
}

func (s *adoptScript) workloadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workloads
}

func newAdoptRuntime(script *adoptScript) runtime.Runtime {
	script.inner = fake.New()
	return &adoptRuntime{script: script}
}

type adoptRuntime struct {
	script *adoptScript
}

func (r *adoptRuntime) Name() types.RuntimeName { return r.script.inner.Name() }

func (r *adoptRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.script.inner.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &adoptMachine{Machine: m, token: spec.AgentToken, script: r.script}, nil
}

type adoptMachine struct {
	runtime.Machine
	token  string
	script *adoptScript
}

func (m *adoptMachine) DialAgent(ctx context.Context) (*guest.Client, error) {
	a, b := net.Pipe()
	go m.script.serve(a)
	return guest.Dial(ctx, b, m.token, "test-host")
}

func (s *adoptScript) serve(rw io.ReadWriteCloser) {
	defer rw.Close()
	for {
		env, err := guest.ReadFrame(rw)
		if err != nil {
			return
		}
		s.note(env.Method)
		var werr error
		switch env.Method {
		case guest.MethodHandshake:
			res := guest.HandshakeRes{OK: true, AgentVersion: "test", Protocol: guest.ProtocolVersion}
			if s.mode != "nocap" {
				res.Capabilities = guest.GuestCapabilities()
			}
			werr = writeGuestResult(rw, env, res)
		case guest.MethodReady:
			werr = writeGuestResult(rw, env, guest.ReadyRes{Ready: true})
		case guest.MethodHealth:
			werr = writeGuestResult(rw, env, guest.HealthRes{OK: true})
		case guest.MethodAdopt:
			werr = s.serveAdopt(rw, env)
		case guest.MethodEphemeralKeychain:
			werr = writeEphemeralKeychain(rw, env)
		case guest.MethodMaterialize:
			werr = writeGuestResult(rw, env, guest.MaterializeRes{OK: true, Placed: []string{"ed"}})
		case guest.MethodNetInfo:
			werr = writeGuestResult(rw, env, guest.NetInfoRes{PrimaryIP: "192.168.64.9"})
		case guest.MethodWorkload:
			werr = writeGuestResult(rw, env, guest.WorkloadRes{OK: true})
		default:
			werr = guest.WriteFrame(rw, guest.Envelope{
				V: guest.ProtocolVersion, ID: env.ID, Kind: guest.KindResponse, Method: env.Method,
			})
		}
		if werr != nil {
			return
		}
	}
}

func (s *adoptScript) serveAdopt(rw io.ReadWriteCloser, env guest.Envelope) error {
	if s.mode == "unknown" || s.mode == "nocap" {
		return guest.WriteFrame(rw, guest.Envelope{
			V: guest.ProtocolVersion, ID: env.ID, Kind: guest.KindError, Method: env.Method,
			Error: &guest.CallError{Code: "unknown_method", Message: env.Method},
		})
	}
	if s.mode == "error" {
		return guest.WriteFrame(rw, guest.Envelope{
			V: guest.ProtocolVersion, ID: env.ID, Kind: guest.KindError, Method: env.Method,
			Error: &guest.CallError{Code: "adopt", Message: "injected adopt failure"},
		})
	}
	req, err := guest.DecodePayload[guest.AdoptReq](env)
	if err != nil {
		return err
	}
	var res guest.AdoptRes
	switch s.mode {
	case "zero":
		res, err = guest.AcceptedAdopt(req, bytes.Repeat([]byte{0}, guest.AdoptSampleLen), adoptScriptUUID)
	case "dup":
		res, err = guest.AcceptedAdopt(req, bytes.Repeat([]byte{0x11}, guest.AdoptSampleLen), adoptScriptUUID)
	default:
		res, err = guest.PerformAdopt(req, guest.NewSoftwarePlatform())
	}
	if err != nil {
		return guest.WriteFrame(rw, guest.Envelope{
			V: guest.ProtocolVersion, ID: env.ID, Kind: guest.KindError, Method: env.Method,
			Error: &guest.CallError{Code: "adopt", Message: err.Error()},
		})
	}
	res.ClockUnixNano = time.Now().UnixNano()
	return writeGuestResult(rw, env, res)
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}
