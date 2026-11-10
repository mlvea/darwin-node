package engine

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

// Guest Materialize is fail-closed. OK:false and an RPC error must fail the
// pod before it is Running, stop the VM, and release the slot.

func materializeEngine(t *testing.T, rt runtime.Runtime) *Engine {
	t.Helper()
	leakcheck.Check(t)
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)
	return e
}

func podWithVolume(name, uid string) *corev1.Pod {
	p := samplePod(name, uid)
	p.Spec.Volumes = []corev1.Volume{{
		Name:         "ed",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name:      "ed",
		MountPath: "/data",
	}}
	return p
}

func waitMaterializeSettled(t *testing.T, e *Engine, rt *fake.Runtime, name string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", name)
		if err == nil && rt.MaterializeCalls() > 0 && (p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodFailed) {
			return p
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ := e.Get("default", name)
	phase := corev1.PodPhase("missing")
	msg := ""
	if p != nil {
		phase = p.Status.Phase
		msg = p.Status.Message
	}
	t.Fatalf("materialize did not settle: calls=%d phase=%s msg=%q", rt.MaterializeCalls(), phase, msg)
	return nil
}

func waitPhaseSettled(t *testing.T, e *Engine, name string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		p, err := e.Get("default", name)
		if err == nil && (p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodFailed) {
			return p
		}
		time.Sleep(15 * time.Millisecond)
	}
	p, _ := e.Get("default", name)
	phase := corev1.PodPhase("missing")
	msg := ""
	if p != nil {
		phase = p.Status.Phase
		msg = p.Status.Message
	}
	t.Fatalf("pod %s did not settle: phase=%s msg=%q", name, phase, msg)
	return nil
}

func waitVMReaped(t *testing.T, e *Engine, rt *fake.Runtime, name, uid string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	dir := filepath.Join(e.cfg.CacheDir, "pods", uid)
	for time.Now().Before(deadline) {
		e.mu.RLock()
		rec := e.pods[Key("default", name)]
		e.mu.RUnlock()
		var st types.VMState
		if rec != nil {
			rec.mu.Lock()
			if rec.machine != nil {
				st = rec.machine.Status().State
			}
			rec.mu.Unlock()
		}
		_, statErr := os.Stat(dir)
		if st == types.VMStopped && e.slots.Used() == 0 && os.IsNotExist(statErr) && rt.Running() == 0 {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("pod %s not reaped: slots=%d runningVMs=%d", name, e.slots.Used(), rt.Running())
}

func TestMaterializeNotOKFailsPodAndStopsVM(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    string
	}{
		{name: "message", message: "placement refused", want: "placement refused"},
		{name: "empty", message: "", want: "guest refused volume placement"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := fake.New()
			rt.MaterializeFn = func(guest.MaterializeReq) guest.MaterializeRes {
				return guest.MaterializeRes{OK: false, Message: tc.message}
			}
			e := materializeEngine(t, rt)
			if err := e.Create(context.Background(), podWithVolume("mat", "uid-mat"), Credentials{}); err != nil {
				t.Fatal(err)
			}
			p := waitMaterializeSettled(t, e, rt, "mat")
			if p.Status.Phase != corev1.PodFailed {
				t.Fatalf("phase %s msg=%q, materialize OK:false left the pod running", p.Status.Phase, p.Status.Message)
			}
			if !strings.Contains(p.Status.Message, "materialize") || !strings.Contains(p.Status.Message, tc.want) {
				t.Fatalf("message %q, want materialize and %q", p.Status.Message, tc.want)
			}
			if podReady(p) {
				t.Fatal("failed pod still PodReady")
			}
			waitVMReaped(t, e, rt, "mat", "uid-mat")
			if err := e.Create(context.Background(), samplePod("mat-next", "uid-mat-next"), Credentials{}); err != nil {
				t.Fatalf("slot not reusable: %v", err)
			}
			waitPhase(t, e, "default", "mat-next", corev1.PodRunning)
			_ = e.Delete(context.Background(), "default", "mat-next", 0)
		})
	}
}

func TestMaterializeRPCErrorFailsPodAndStopsVM(t *testing.T) {
	inner := fake.New()
	closed := make(chan struct{})
	rt := &materializeRPCRuntime{Runtime: inner, closed: closed}
	e := materializeEngine(t, rt)
	if err := e.Create(context.Background(), podWithVolume("rpc", "uid-rpc"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhaseSettled(t, e, "rpc")
	if p.Status.Phase != corev1.PodFailed {
		t.Fatalf("phase %s msg=%q, materialize RPC error left the pod running", p.Status.Phase, p.Status.Message)
	}
	if !strings.Contains(p.Status.Message, "share missing") {
		t.Fatalf("message %q, want the RPC error", p.Status.Message)
	}
	if podReady(p) {
		t.Fatal("failed pod still PodReady")
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("guest agent connection still open after materialize RPC error")
	}
	waitVMReaped(t, e, inner, "rpc", "uid-rpc")
	if err := e.Create(context.Background(), samplePod("rpc-next", "uid-rpc-next"), Credentials{}); err != nil {
		t.Fatalf("slot not reusable: %v", err)
	}
	waitPhase(t, e, "default", "rpc-next", corev1.PodRunning)
	_ = e.Delete(context.Background(), "default", "rpc-next", 0)
}

// materializeRPCRuntime completes Ready, then returns a protocol error for
// Materialize. NetInfo is answered so a warn-and-continue bug is visible as
// Running instead of a hung dial. Only the first dial is tracked: the pod
// created after the slot is released dials again and must not re-close it.
type materializeRPCRuntime struct {
	*fake.Runtime
	mu     sync.Mutex
	closed chan struct{}
	seen   bool
}

func (r *materializeRPCRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &materializeRPCMachine{Machine: m, token: spec.AgentToken, rt: r}, nil
}

type materializeRPCMachine struct {
	runtime.Machine
	token string
	rt    *materializeRPCRuntime
}

func (m *materializeRPCMachine) DialAgent(ctx context.Context) (*guest.Client, error) {
	a, b := net.Pipe()
	go serveMaterializeRPCError(a)
	return guest.Dial(ctx, m.rt.track(b), m.token, "test-host")
}

func (r *materializeRPCRuntime) track(conn net.Conn) net.Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen {
		return conn
	}
	r.seen = true
	return &trackConn{Conn: conn, closed: r.closed}
}

func serveMaterializeRPCError(rw io.ReadWriteCloser) {
	defer rw.Close()
	for {
		env, err := guest.ReadFrame(rw)
		if err != nil {
			return
		}
		var werr error
		switch env.Method {
		case guest.MethodHandshake:
			werr = writeGuestResult(rw, env, guest.HandshakeRes{OK: true, AgentVersion: "test", Protocol: guest.ProtocolVersion})
		case guest.MethodReady:
			werr = writeGuestResult(rw, env, guest.ReadyRes{Ready: true})
		case guest.MethodMaterialize:
			werr = guest.WriteFrame(rw, guest.Envelope{
				V: guest.ProtocolVersion, ID: env.ID, Kind: guest.KindError, Method: env.Method,
				Error: &guest.CallError{Code: "materialize", Message: "share missing"},
			})
		case guest.MethodNetInfo:
			werr = writeGuestResult(rw, env, guest.NetInfoRes{PrimaryIP: "192.168.64.9"})
		default:
			werr = guest.WriteFrame(rw, guest.Envelope{V: guest.ProtocolVersion, ID: env.ID, Kind: guest.KindResponse, Method: env.Method})
		}
		if werr != nil {
			return
		}
	}
}

func writeGuestResult(rw io.Writer, env guest.Envelope, payload any) error {
	body, err := guest.EncodePayload(payload)
	if err != nil {
		return err
	}
	return guest.WriteFrame(rw, guest.Envelope{
		V: guest.ProtocolVersion, ID: env.ID, Kind: guest.KindResponse, Method: env.Method, Payload: body,
	})
}
