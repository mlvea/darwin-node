package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/image"
	"github.com/darwin-node/darwin-node/pkg/runtime"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestWarmAdoptRequiresMatchingSize(t *testing.T) {
	e, rt, rec := warmEngine(t, 2, 1, warmImg)
	waitFor(t, "warm VM", func() bool { return e.warm.len() == 1 })
	pod := samplePod("big", "uid-big")
	pod.Spec.Containers[0].Image = warmImg
	pod.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("8Gi"),
	}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "big", corev1.PodRunning)
	if rec.has(event.ReasonWarmAdopted) {
		t.Fatal("adopted a 2 vCPU / 4Gi warm VM for a pod that requested 4 vCPU / 8Gi")
	}
	if rt.Starts() < 2 {
		t.Fatalf("size mismatch must cold-boot, starts=%d", rt.Starts())
	}
	if e.warm.len() != 1 {
		t.Fatalf("warm pool len=%d, the mismatched VM should stay warm", e.warm.len())
	}
}

func TestRealPodPreemptsInProgressWarmBoot(t *testing.T) {
	release := make(chan struct{})
	rt := &blockCreateRuntime{
		Runtime: fake.New(),
		entered: make(chan struct{}),
		release: release,
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	slots, err := capacity.New(1)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 5 * time.Second
	cfg.AllowNATWorkloads = true
	cfg.WarmSlots = 0
	cfg.WarmImage = warmImg
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	t.Cleanup(func() { _ = e.Close() })

	bootErr := make(chan error, 1)
	go func() { bootErr <- e.bootWarm(context.Background(), warmImg) }()
	select {
	case <-rt.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("warm boot did not reach Create")
	}
	if e.slots.Used() != 1 {
		t.Fatalf("in-progress warm boot slots=%d, want 1", e.slots.Used())
	}
	pod := samplePod("real", "uid-real")
	pod.Spec.Containers[0].Image = "local/macos:other"
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatalf("real pod rejected while a warm boot held the only slot: %v", err)
	}
	waitPhase(t, e, "default", "real", corev1.PodRunning)
	if rt.Running() != 1 || e.slots.Used() != 1 {
		t.Fatalf("running=%d slots=%d, want the pod's VM only", rt.Running(), e.slots.Used())
	}
	select {
	case err := <-bootErr:
		if err == nil {
			t.Fatal("preempted warm boot reported success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("warm boot did not return after the pod preempted it")
	}
}

func TestCrashedWarmVMIsRemoved(t *testing.T) {
	e, _, _ := warmEngine(t, 1, 0, warmImg)
	mustBootWarm(t, e)
	snap := e.warm.snapshot()
	if len(snap) != 1 || snap[0].machine == nil {
		t.Fatalf("warm entry %+v", snap)
	}
	if err := snap[0].machine.Stop(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if e.warm.len() != 1 || e.slots.Used() != 1 {
		t.Fatalf("setup len=%d slots=%d", e.warm.len(), e.slots.Used())
	}
	e.fillWarm(context.Background())
	if e.warm.len() != 0 || e.slots.Used() != 0 {
		t.Fatalf("crashed warm VM stayed pooled: len=%d slots=%d", e.warm.len(), e.slots.Used())
	}
	if err := e.Create(context.Background(), samplePod("next", "uid-next"), Credentials{}); err != nil {
		t.Fatalf("pod rejected after a crashed warm VM: %v", err)
	}
	waitPhase(t, e, "default", "next", corev1.PodRunning)
}

type countCreateRuntime struct {
	*fake.Runtime
	n atomic.Int32
}

func (r *countCreateRuntime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	r.n.Add(1)
	return nil, errors.New("warm boot failed")
}

func TestWarmBootFailureUsesExponentialBackoff(t *testing.T) {
	slots, err := capacity.New(1)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Runtime = "fake"
	cfg.CacheDir = t.TempDir()
	cfg.AgentReadyTimeout = 2 * time.Second
	cfg.AllowNATWorkloads = true
	cfg.WarmSlots = 0
	cfg.WarmImage = warmImg
	rt := &countCreateRuntime{Runtime: fake.New()}
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	clk := newManualClock(time.Now())
	e.clock = clk
	t.Cleanup(func() { _ = e.Close() })
	// The replenisher is not running (WarmSlots was 0 at New). Raising it
	// here only changes the budget fillWarm reads.
	e.cfg.WarmSlots = 1

	e.fillWarm(context.Background())
	if got := rt.n.Load(); got != 1 {
		t.Fatalf("boots=%d, want the first attempt", got)
	}
	e.fillWarm(context.Background())
	if got := rt.n.Load(); got != 1 {
		t.Fatalf("boots=%d, warm failure retried immediately", got)
	}
	clk.Advance(time.Second)
	e.fillWarm(context.Background())
	if got := rt.n.Load(); got != 2 {
		t.Fatalf("boots=%d, want a retry after 1s", got)
	}
	clk.Advance(time.Second)
	e.fillWarm(context.Background())
	if got := rt.n.Load(); got != 2 {
		t.Fatalf("boots=%d, backoff did not double to 2s", got)
	}
	clk.Advance(time.Second)
	e.fillWarm(context.Background())
	if got := rt.n.Load(); got != 3 {
		t.Fatalf("boots=%d, want a retry after the 2s backoff", got)
	}
}

func TestWarmAdoptRechecksPullCredentials(t *testing.T) {
	ref := "ghcr.io/secret/macos:15"
	e, rt, rec := warmEngine(t, 2, 1, ref)
	waitFor(t, "warm VM", func() bool { return e.warm.len() == 1 })
	writeCommittedBake(t, image.CacheDir(e.cfg.CacheDir, ref), "SECRET-DISK")
	e.puller = &scriptedPuller{err: errors.New("unauthorized")}
	pod := samplePod("hot", "uid-hot")
	pod.Spec.Containers[0].Image = ref
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "hot", corev1.PodRunning)
	if rec.has(event.ReasonWarmAdopted) {
		t.Fatal("adopted a warm VM whose image this pod is not allowed to pull")
	}
	if rt.Starts() < 2 {
		t.Fatalf("unauthorized adopt must cold-boot, starts=%d", rt.Starts())
	}
}

func TestWarmAdoptRotatesAgentToken(t *testing.T) {
	e, rt, _ := warmEngine(t, 2, 0, warmImg)
	mustBootWarm(t, e)
	snap := e.warm.snapshot()
	if len(snap) != 1 {
		t.Fatalf("warm len=%d", len(snap))
	}
	path := filepath.Join(snap[0].root, "control", types.GuestAgentTokenFile)
	old, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	oldTok := strings.TrimSpace(string(old))
	if oldTok == "" {
		t.Fatal("warm boot has an empty agent token")
	}
	pod := samplePod("hot", "uid-hot")
	pod.Spec.Containers[0].Image = warmImg
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "hot", corev1.PodRunning)
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	newTok := strings.TrimSpace(string(got))
	if newTok == "" || newTok == oldTok {
		t.Fatalf("adopted VM kept the warm-boot token %q", oldTok)
	}
	if rt.AgentToken() != newTok {
		t.Fatalf("guest token %q, control share %q", rt.AgentToken(), newTok)
	}
}
