package engine

import (
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

func TestEphemeralKeychainBeforeWorkload(t *testing.T) {
	script := &keychainScript{mode: "ok"}
	e := materializeEngine(t, newKeychainRuntime(script))
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
	keychain := indexOf(methods, guest.MethodEphemeralKeychain)
	work := indexOf(methods, guest.MethodWorkload)
	if ready < 0 || adopt < 0 || mat < 0 || keychain < 0 || work < 0 {
		t.Fatalf("methods %v", methods)
	}
	if ready >= adopt || adopt >= mat || mat >= keychain || keychain >= work {
		t.Fatalf("order %v", methods)
	}
	_ = e.Delete(context.Background(), "default", "ord", 0)
}

func TestEphemeralKeychainStepErrorsFailClosed(t *testing.T) {
	steps := []struct {
		step string
		want string
	}{
		{step: "home", want: "ephemeral keychain: home"},
		{step: "create", want: "ephemeral keychain: create"},
		{step: "unlock", want: "ephemeral keychain: unlock"},
		{step: "default", want: "ephemeral keychain: default"},
		{step: "search", want: "ephemeral keychain: search list"},
		{step: "hide", want: "ephemeral keychain: hide login"},
	}
	for _, step := range steps {
		t.Run(step.step, func(t *testing.T) {
			kc := guest.NewImageKeychains()
			kc.FailAt(step.step)
			assertKeychainStartFailed(t, &keychainScript{mode: "ok", kc: kc}, "uid-bad-"+step.step, step.want)
		})
	}
}

func TestEphemeralKeychainMissingCapabilityFailsClosed(t *testing.T) {
	t.Run("missing capability", func(t *testing.T) {
		assertKeychainStartFailed(t, &keychainScript{mode: "nocap"}, "uid-nocap", "does not implement ephemeral keychain")
	})
	t.Run("unknown method", func(t *testing.T) {
		assertKeychainStartFailed(t, &keychainScript{mode: "unknown"}, "uid-unknown", "does not implement ephemeral keychain")
	})
}

func TestEphemeralKeychainLegacyAllowSkipsOnlyMissingMethod(t *testing.T) {
	t.Run("opt in", func(t *testing.T) {
		t.Setenv(envAllowLegacyGuest, "1")
		script := &keychainScript{mode: "nocap"}
		e := materializeEngine(t, newKeychainRuntime(script))
		pod := samplePod("old", "uid-old")
		pod.Spec.Containers[0].Command = []string{"/bin/true"}
		if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
			t.Fatal(err)
		}
		waitPhase(t, e, "default", "old", corev1.PodRunning)
		if script.workloadCount() != 1 {
			t.Fatalf("workload calls %d", script.workloadCount())
		}
		_ = e.Delete(context.Background(), "default", "old", 0)
	})
	t.Run("step error still fails", func(t *testing.T) {
		t.Setenv(envAllowLegacyGuest, "true")
		kc := guest.NewImageKeychains()
		kc.FailAt("create")
		assertKeychainStartFailed(t, &keychainScript{mode: "ok", kc: kc}, "uid-legacy-create", "ephemeral keychain: create")
	})
}

func assertKeychainStartFailed(t *testing.T, script *keychainScript, uid, want string) {
	t.Helper()
	e := materializeEngine(t, newKeychainRuntime(script))
	pod := samplePod("bad", uid)
	pod.Spec.Containers[0].Command = []string{"/bin/true"}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhaseSettled(t, e, "bad")
	if p.Status.Phase != corev1.PodFailed || !strings.Contains(p.Status.Message, want) {
		t.Fatalf("phase %s msg %q, want %q", p.Status.Phase, p.Status.Message, want)
	}
	if script.workloadCount() != 0 {
		t.Fatalf("workload calls %d after %s", script.workloadCount(), want)
	}
	waitVMReaped(t, e, script.inner, "bad", uid)
}

type keychainScript struct {
	mu        sync.Mutex
	methods   []string
	workloads int
	mode      string
	kc        *guest.MemoryKeychain
	inner     *fake.Runtime
}

func (s *keychainScript) note(method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.methods = append(s.methods, method)
	if method == guest.MethodWorkload {
		s.workloads++
	}
}

func (s *keychainScript) snapshot() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.methods...)
}

func (s *keychainScript) workloadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.workloads
}

func newKeychainRuntime(script *keychainScript) runtime.Runtime {
	script.inner = fake.New()
	return &keychainRuntime{script: script}
}

type keychainRuntime struct {
	script *keychainScript
}

func (r *keychainRuntime) Name() types.RuntimeName { return r.script.inner.Name() }

func (r *keychainRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.script.inner.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &keychainMachine{Machine: m, token: spec.AgentToken, script: r.script}, nil
}

type keychainMachine struct {
	runtime.Machine
	token  string
	script *keychainScript
}

func (m *keychainMachine) DialAgent(ctx context.Context) (*guest.Client, error) {
	a, b := net.Pipe()
	go m.script.serve(a)
	return guest.Dial(ctx, b, m.token, "test-host")
}

func (s *keychainScript) serve(rw io.ReadWriteCloser) {
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
			} else {
				for _, name := range guest.GuestCapabilities() {
					if name == guest.CapEphemeralKeychain {
						continue
					}
					res.Capabilities = append(res.Capabilities, name)
				}
			}
			werr = writeGuestResult(rw, env, res)
		case guest.MethodReady:
			werr = writeGuestResult(rw, env, guest.ReadyRes{Ready: true})
		case guest.MethodHealth:
			werr = writeGuestResult(rw, env, guest.HealthRes{OK: true})
		case guest.MethodAdopt:
			werr = s.serveKeychainAdopt(rw, env)
		case guest.MethodEphemeralKeychain:
			werr = s.serveKeychain(rw, env)
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

func (s *keychainScript) serveKeychainAdopt(rw io.ReadWriteCloser, env guest.Envelope) error {
	req, err := guest.DecodePayload[guest.AdoptReq](env)
	if err != nil {
		return err
	}
	res, err := guest.PerformAdopt(req, guest.NewSoftwarePlatform())
	if err != nil {
		return guest.WriteFrame(rw, guest.Envelope{
			V: guest.ProtocolVersion, ID: env.ID, Kind: guest.KindError, Method: env.Method,
			Error: &guest.CallError{Code: "adopt", Message: err.Error()},
		})
	}
	res.ClockUnixNano = time.Now().UnixNano()
	return writeGuestResult(rw, env, res)
}

func (s *keychainScript) serveKeychain(rw io.ReadWriteCloser, env guest.Envelope) error {
	if s.mode == "unknown" || s.mode == "nocap" {
		return guest.WriteFrame(rw, guest.Envelope{
			V: guest.ProtocolVersion, ID: env.ID, Kind: guest.KindError, Method: env.Method,
			Error: &guest.CallError{Code: "unknown_method", Message: env.Method},
		})
	}
	return writeEphemeralKeychainOn(rw, env, s.kc)
}

func writeEphemeralKeychain(rw io.ReadWriteCloser, env guest.Envelope) error {
	return writeEphemeralKeychainOn(rw, env, nil)
}

func writeEphemeralKeychainOn(rw io.ReadWriteCloser, env guest.Envelope, kc *guest.MemoryKeychain) error {
	req, err := guest.DecodePayload[guest.EphemeralKeychainReq](env)
	if err != nil {
		return err
	}
	var platform guest.KeychainPlatform
	if kc != nil {
		platform = kc
	} else {
		platform = guest.NewImageKeychains()
	}
	res, err := guest.InstallEphemeralKeychain(req, platform)
	if err != nil {
		return guest.WriteFrame(rw, guest.Envelope{
			V: guest.ProtocolVersion, ID: env.ID, Kind: guest.KindError, Method: env.Method,
			Error: &guest.CallError{Code: "keychain", Message: err.Error()},
		})
	}
	return writeGuestResult(rw, env, res)
}
