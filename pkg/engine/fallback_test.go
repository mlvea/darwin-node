package engine

import (
	"context"
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

// hangDialRuntime's machines block in DialAgent until the context ends, so
// the engine must still reach the guest over TCP with a fresh budget.
type hangDialRuntime struct{ *fake.Runtime }

func (r *hangDialRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	m, err := r.Runtime.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	return &hangDialMachine{Machine: m}, nil
}

type hangDialMachine struct{ runtime.Machine }

func (m *hangDialMachine) DialAgent(ctx context.Context) (*guest.Client, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (m *hangDialMachine) Status() types.VMStatus {
	st := m.Machine.Status()
	st.IP = "127.0.0.1"
	return st
}

func TestTCPFallbackAfterExhaustedVsockBudget(t *testing.T) {
	leakcheck.Check(t)
	ln, err := net.Listen("tcp", "127.0.0.1:1050")
	if err != nil {
		t.Skipf("guest tcp port unavailable: %v", err)
	}
	srvCtx, srvCancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		srvCancel()
		_ = ln.Close()
	})

	oldDial := agentDialTimeout
	agentDialTimeout = 200 * time.Millisecond
	t.Cleanup(func() { agentDialTimeout = oldDial })

	slots, err := capacity.New(1)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 3 * time.Second
	cfg.AllowNATWorkloads = true
	e := New(cfg, slots, &hangDialRuntime{Runtime: fake.New()}, sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(e.Close)

	uid := "uid-fb"
	var accepted sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		accepted.Lock()
		defer accepted.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Lock()
			conns = append(conns, c)
			accepted.Unlock()
			go func(c net.Conn) {
				defer c.Close()
				tokPath := filepath.Join(cfg.CacheDir, "pods", uid, "control", types.GuestAgentTokenFile)
				var tok string
				deadline := time.Now().Add(2 * time.Second)
				for time.Now().Before(deadline) && tok == "" {
					b, err := os.ReadFile(tokPath)
					if err == nil {
						tok = strings.TrimSpace(string(b))
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
				h := guest.Handler{Token: tok, AgentVersion: "tcp", IdleTimeout: 500 * time.Millisecond}
				h.Init()
				_ = guest.Serve(srvCtx, c, h)
			}(c)
		}
	}()

	pod := samplePod("fb", uid)
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "fb", corev1.PodRunning)
	if p.Status.Phase != corev1.PodRunning {
		t.Fatalf("phase %s (%s)", p.Status.Phase, p.Status.Message)
	}
	_ = e.Delete(context.Background(), "default", "fb", 0)
}
