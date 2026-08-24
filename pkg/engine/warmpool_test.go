package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"

	corev1 "k8s.io/api/core/v1"
)

type captureRecorder struct {
	mu      sync.Mutex
	reasons []string
}

func (c *captureRecorder) Normal(_ context.Context, reason, _ string) {
	c.mu.Lock()
	c.reasons = append(c.reasons, reason)
	c.mu.Unlock()
}

func (c *captureRecorder) Warn(context.Context, string, string) {}

func (c *captureRecorder) has(reason string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.reasons {
		if r == reason {
			return true
		}
	}
	return false
}

func warmEngine(t *testing.T, slots int, warmSlots int, img string) (*Engine, *fake.Runtime, *captureRecorder) {
	t.Helper()
	slotTable, err := capacity.New(slots)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	cfg.WarmSlots = warmSlots
	cfg.WarmImage = img
	rt := fake.New()
	rec := &captureRecorder{}
	e := New(cfg, slotTable, rt, sidecar.None{}, rec, "10.0.0.1")
	t.Cleanup(e.Close)
	return e, rt, rec
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func mountedPod(name, uid, img string) *corev1.Pod {
	pod := samplePod(name, uid)
	pod.Spec.Containers[0].Image = img
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "sec",
		VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "s"}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "sec", MountPath: "/etc/sec"}}
	return pod
}

var secretCreds = Credentials{Secrets: map[string]*corev1.Secret{
	"s": {Data: map[string][]byte{"token": []byte("shh")}},
}}

const warmImg = "local/macos:warm"

func TestWarmPoolBootsInFreeSlotOnly(t *testing.T) {
	e, rt, rec := warmEngine(t, 2, 1, warmImg)
	waitFor(t, "warm VM boot", func() bool { return e.warm.len() == 1 })
	if got := e.slots.Used(); got != 1 {
		t.Fatalf("warm VM must hold a real slot, used=%d", got)
	}
	if rt.Starts() != 1 {
		t.Fatalf("starts=%d", rt.Starts())
	}

	// A non-adoptable pod takes the remaining free slot without touching the pool.
	if err := e.Create(context.Background(), mountedPod("cold", "uid-cold", warmImg), secretCreds); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "cold", corev1.PodRunning)
	if got := rt.Starts(); got != 2 {
		t.Fatalf("cold boot expected, starts=%d", got)
	}

	// A third, non-adoptable pod reclaims the warm slot instead of being queued.
	if err := e.Create(context.Background(), samplePod("third", "uid-third"), Credentials{}); err != nil {
		t.Fatalf("third pod must reclaim the warm slot: %v", err)
	}
	_ = waitPhase(t, e, "default", "third", corev1.PodRunning)
	if !rec.has(event.ReasonWarmEvicted) {
		t.Fatal("expected WarmVMEvicted event")
	}

	// With no warm entries left to reclaim, a fourth demand fails closed.
	err := e.Create(context.Background(), samplePod("fourth", "uid-fourth"), Credentials{})
	if err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("expected capacity rejection, got %v", err)
	}
	for _, n := range []string{"cold", "third"} {
		_ = e.Delete(context.Background(), "default", n, 0)
	}
}

func TestWarmPoolAdoptionAvoidsColdBoot(t *testing.T) {
	e, rt, rec := warmEngine(t, 2, 1, warmImg)
	waitFor(t, "warm VM boot", func() bool { return e.warm.len() == 1 })

	pod := samplePod("hot", "uid-hot")
	pod.Spec.Containers[0].Image = warmImg
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	p := waitPhase(t, e, "default", "hot", corev1.PodRunning)
	if !rec.has(event.ReasonWarmAdopted) {
		t.Fatalf("pod should adopt a warm VM; events show otherwise")
	}
	if rt.Starts() != 1 {
		t.Fatalf("adoption must not boot a VM, starts=%d", rt.Starts())
	}
	if p.Status.PodIP == "" {
		t.Fatal("adopted pod missing IP")
	}
	_ = e.Delete(context.Background(), "default", "hot", 0)
}

func TestWarmPoolSkipsPodsWithMounts(t *testing.T) {
	e, rt, _ := warmEngine(t, 2, 1, warmImg)
	waitFor(t, "warm VM boot", func() bool { return e.warm.len() == 1 })

	if err := e.Create(context.Background(), mountedPod("mnt", "uid-mnt", warmImg), secretCreds); err != nil {
		t.Fatal(err)
	}
	_ = waitPhase(t, e, "default", "mnt", corev1.PodRunning)
	if rt.Starts() != 2 {
		t.Fatalf("mounted pod must cold-boot, starts=%d", rt.Starts())
	}
	_ = e.Delete(context.Background(), "default", "mnt", 0)
}

func TestWarmReclaimUnderCapacityPressure(t *testing.T) {
	// maxVMs=1: the single slot goes warm first, so the first real pod must
	// reclaim it rather than be rejected.
	e, rt, rec := warmEngine(t, 1, 1, warmImg)
	waitFor(t, "warm VM boot", func() bool { return e.warm.len() == 1 })

	if err := e.Create(context.Background(), mountedPod("busy", "uid-busy", warmImg), secretCreds); err != nil {
		t.Fatalf("pod must reclaim the warm slot: %v", err)
	}
	_ = waitPhase(t, e, "default", "busy", corev1.PodRunning)
	if !rec.has(event.ReasonWarmEvicted) {
		t.Fatal("expected WarmVMEvicted event")
	}
	if rt.Starts() != 2 {
		t.Fatalf("reclaimed slot must cold-boot a fresh VM, starts=%d", rt.Starts())
	}
	if e.slots.Used() != 1 {
		t.Fatalf("slot leak: used=%d", e.slots.Used())
	}
	_ = e.Delete(context.Background(), "default", "busy", 0)
}

func TestCloseStopsWarmVMsAndFreesSlots(t *testing.T) {
	e, _, _ := warmEngine(t, 2, 2, warmImg)
	waitFor(t, "two warm VMs", func() bool { return e.warm.len() == 2 })
	e.Close()
	if e.slots.Used() != 0 {
		t.Fatalf("close must release warm slots, used=%d", e.slots.Used())
	}
	if e.warm.len() != 0 {
		t.Fatalf("pool not drained: %d", e.warm.len())
	}
}
