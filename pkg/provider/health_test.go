package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/node"
	rtfake "github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

func TestCreatePodRejectedWhileNodeNotReadyThenAdmitted(t *testing.T) {
	slots, err := capacity.New(2)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.CacheDir = t.TempDir()
	cfg.Runtime = types.RuntimeFake
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	eng := engine.New(cfg, slots, rtfake.New(), sidecar.None{}, event.Nop{}, "10.0.0.8")
	t.Cleanup(func() { _ = eng.Close() })
	inv := node.Inventory{
		Host: node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30, DiskUsedFrac: 0.1, MemoryUsedFrac: 0.1},
		Cfg:  cfg,
	}
	p := New(cfg, eng, inv, event.Nop{})
	np := node.NewStatusProvider(inv)
	failed := true
	np.Probe = func(context.Context) (node.Host, error) {
		if failed {
			return node.Host{}, errors.New("disk probe: disk unavailable")
		}
		h := inv.Host
		h.DiskUsedFrac = 0.1
		return h, nil
	}
	if err := np.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	p.SetHealth(np)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "macos", Namespace: "default", UID: k8stypes.UID("u-notready")},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "local/macos:test"}}},
	}
	err = p.CreatePod(context.Background(), pod)
	if err == nil || !strings.Contains(err.Error(), "not ready") {
		t.Fatalf("create while the host probe was failing: %v", err)
	}
	pods, err := p.GetPods(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 0 || slots.Used() != 0 {
		t.Fatalf("rejected create admitted pods=%d slots=%d", len(pods), slots.Used())
	}

	failed = false
	if err := np.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := p.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("create after the probe recovered: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, err := p.GetPodStatus(context.Background(), "default", "macos")
		if err == nil && st.Phase == corev1.PodRunning {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	st, _ := p.GetPodStatus(context.Background(), "default", "macos")
	t.Fatalf("pod never running after recovery: %+v", st)
}
