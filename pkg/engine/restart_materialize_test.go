package engine

import (
	"context"
	"io"
	"net"
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

// A liveness restart rematerializes copy-mode volumes. OK:false and an RPC
// error must fail the pod, stop the VM, and release the slot. Warning and
// returning nil leaves the pod Running with those volumes missing.

func restartMatEngine(t *testing.T, rt runtime.Runtime) *Engine {
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
	cfg.ProbeInterval = 20 * time.Millisecond
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)
	return e
}

func podWithCopyVolume(name, uid string) *corev1.Pod {
	p := samplePod(name, uid)
	p.Spec.RestartPolicy = corev1.RestartPolicyAlways
	p.Spec.Volumes = []corev1.Volume{{
		Name:         "sec",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}},
	}}
	p.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "sec", MountPath: "/etc/sec"}}
	p.Spec.Containers[0].LivenessProbe = &corev1.Probe{
		ProbeHandler:     corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"dead"}}},
		PeriodSeconds:    1,
		TimeoutSeconds:   1,
		FailureThreshold: 1,
	}
	return p
}

func copySecretCreds() Credentials {
	return Credentials{Secrets: map[string]*corev1.Secret{
		"s": {Data: map[string][]byte{"token": []byte("shh")}},
	}}
}

func deadExec(ctx context.Context, req guest.ExecReq, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if len(req.Argv) > 0 && req.Argv[0] == "dead" {
		return 1, nil
	}
	return 0, nil
}

func TestRestartVMMaterializeNotOKFailsPod(t *testing.T) {
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
			rt.ExecFn = deadExec
			var calls int
			var mu sync.Mutex
			rt.MaterializeFn = func(req guest.MaterializeReq) guest.MaterializeRes {
				mu.Lock()
				calls++
				n := calls
				mu.Unlock()
				if n == 1 {
					placed := make([]string, 0, len(req.Volumes))
					for _, v := range req.Volumes {
						placed = append(placed, v.GuestPath)
					}
					return guest.MaterializeRes{OK: true, Placed: placed}
				}
				return guest.MaterializeRes{OK: false, Message: tc.message}
			}
			e := restartMatEngine(t, rt)
			if err := e.Create(context.Background(), podWithCopyVolume("rs", "uid-rs"), copySecretCreds()); err != nil {
				t.Fatal(err)
			}
			p := waitPhase(t, e, "default", "rs", corev1.PodFailed)
			if rt.Starts() != 2 {
				t.Fatalf("starts=%d, materialize OK:false left the restarted VM running: %s", rt.Starts(), p.Status.Message)
			}
			if !strings.Contains(p.Status.Message, "materialize") || !strings.Contains(p.Status.Message, tc.want) {
				t.Fatalf("phase %s msg=%q, materialize OK:false left the restarted VM running", p.Status.Phase, p.Status.Message)
			}
			if podReady(p) {
				t.Fatal("failed pod still PodReady")
			}
			waitVMReaped(t, e, rt, "rs", "uid-rs")
			if rt.Running() > 2 {
				t.Fatalf("running VMs=%d, cap is 2", rt.Running())
			}
			if err := e.Create(context.Background(), samplePod("rs-next", "uid-rs-next"), Credentials{}); err != nil {
				t.Fatalf("slot not reusable: %v", err)
			}
			waitPhase(t, e, "default", "rs-next", corev1.PodRunning)
			_ = e.Delete(context.Background(), "default", "rs-next", 0)
		})
	}
}

func TestRestartVMMaterializeRPCErrorFailsPod(t *testing.T) {
	inner := fake.New()
	inner.ExecFn = deadExec
	closed := make(chan struct{})
	rt := &restartDialRuntime{Runtime: inner, closed: closed}
	e := restartMatEngine(t, rt)
	if err := e.Create(context.Background(), podWithCopyVolume("rpc", "uid-rpc"), copySecretCreds()); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "rpc", corev1.PodFailed)
	if inner.Starts() != 2 {
		t.Fatalf("starts=%d, materialize RPC error left the restarted VM running: %s", inner.Starts(), p.Status.Message)
	}
	if !strings.Contains(p.Status.Message, "materialize") || !strings.Contains(p.Status.Message, "share missing") {
		t.Fatalf("phase %s msg=%q, materialize RPC error left the restarted VM running", p.Status.Phase, p.Status.Message)
	}
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("guest agent connection still open after restart materialize RPC error")
	}
	waitVMReaped(t, e, inner, "rpc", "uid-rpc")
	if inner.Running() > 2 {
		t.Fatalf("running VMs=%d, cap is 2", inner.Running())
	}
	if err := e.Create(context.Background(), samplePod("rpc-next", "uid-rpc-next"), Credentials{}); err != nil {
		t.Fatalf("slot not reusable: %v", err)
	}
	waitPhase(t, e, "default", "rpc-next", corev1.PodRunning)
	_ = e.Delete(context.Background(), "default", "rpc-next", 0)
}

// restartDialRuntime lets the first agent dial (pod start) use the fake
// guest. The dial after liveness Stop/Start returns a protocol error for
// Materialize and records Close.
type restartDialRuntime struct {
	*fake.Runtime
	closed chan struct{}
}

func (r *restartDialRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &restartDialMachine{Machine: m, token: spec.AgentToken, closed: r.closed}, nil
}

type restartDialMachine struct {
	runtime.Machine
	token  string
	closed chan struct{}
	mu     sync.Mutex
	dials  int
}

func (m *restartDialMachine) DialAgent(ctx context.Context) (*guest.Client, error) {
	m.mu.Lock()
	m.dials++
	n := m.dials
	m.mu.Unlock()
	if n == 1 {
		return m.Machine.DialAgent(ctx)
	}
	a, b := net.Pipe()
	conn := net.Conn(b)
	// Only the restart dial is tracked. Later dials are the bug's extra
	// restarts; they must not close the same channel twice.
	if n == 2 {
		conn = &trackConn{Conn: b, closed: m.closed}
	}
	go serveMaterializeRPCError(a)
	return guest.Dial(ctx, conn, m.token, "test-host")
}
