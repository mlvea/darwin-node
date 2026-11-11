package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/guest"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/volume"

	corev1 "k8s.io/api/core/v1"
)

// Projected-token rematerialize that returns OK:false is a failed refresh.
// The watch already retries a copy that did not land; it must not record
// the token as refreshed.

type refreshEvents struct {
	mu     sync.Mutex
	normal []string
	warn   []string
}

func (r *refreshEvents) Normal(_ context.Context, reason, message string) {
	r.mu.Lock()
	r.normal = append(r.normal, reason+" "+message)
	r.mu.Unlock()
}

func (r *refreshEvents) Warn(_ context.Context, reason, message string) {
	r.mu.Lock()
	r.warn = append(r.warn, reason+" "+message)
	r.mu.Unlock()
}

func (r *refreshEvents) has(kind, reason, frag string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.normal
	if kind == "warn" {
		list = r.warn
	}
	for _, line := range list {
		if strings.Contains(line, reason) && strings.Contains(line, frag) {
			return true
		}
	}
	return false
}

func (r *refreshEvents) count(kind, reason string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	list := r.normal
	if kind == "warn" {
		list = r.warn
	}
	n := 0
	for _, line := range list {
		if strings.Contains(line, reason) {
			n++
		}
	}
	return n
}

func TestProjectedTokenRematerializeNotOKRetries(t *testing.T) {
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
	rt := fake.New()
	var mats atomic.Int32
	rt.MaterializeFn = func(req guest.MaterializeReq) guest.MaterializeRes {
		if mats.Add(1) == 1 {
			placed := make([]string, 0, len(req.Volumes))
			for _, v := range req.Volumes {
				placed = append(placed, v.GuestPath)
			}
			return guest.MaterializeRes{OK: true, Placed: placed}
		}
		return guest.MaterializeRes{OK: false, Message: "token placement refused"}
	}
	ev := &refreshEvents{}
	e := New(cfg, slots, rt, sidecar.None{}, ev, "10.0.0.1")
	t.Cleanup(e.Close)
	e.SetTokenMinter(func(context.Context, *corev1.Pod) ([]volume.ProjectedToken, error) {
		return []volume.ProjectedToken{{
			Volume: "proj", Path: "token", Token: "second",
			ExpiresAt: time.Now().Add(time.Hour),
		}}, nil
	})
	pod := samplePod("ref", "uid-ref")
	pod.Spec.Volumes = []corev1.Volume{{
		Name: "proj",
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}},
		}}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name: "proj", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
	}}
	creds := Credentials{Tokens: []volume.ProjectedToken{{
		Volume: "proj", Path: "token", Token: "first",
		ExpiresAt: time.Now().Add(20 * time.Second),
	}}}
	if err := e.Create(context.Background(), pod, creds); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "ref", corev1.PodRunning)
	tokenPath := filepath.Join(cfg.CacheDir, "pods", "uid-ref", "volumes", "proj", "token")

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ev.has("warn", event.ReasonTokenRefreshFailed, "token placement refused") {
			break
		}
		if ev.has("normal", event.ReasonTokenRefreshed, "") {
			t.Fatal("OK:false was recorded as ServiceAccountTokenRefreshed")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !ev.has("warn", event.ReasonTokenRefreshFailed, "token placement refused") {
		t.Fatal("OK:false did not surface as TokenRefreshFailed")
	}
	time.Sleep(150 * time.Millisecond)
	if ev.has("normal", event.ReasonTokenRefreshed, "") {
		t.Fatal("OK:false was recorded as ServiceAccountTokenRefreshed")
	}
	if ev.count("warn", event.ReasonTokenRefreshFailed) < 2 {
		t.Fatalf("token rematerialize was not retried: warns=%d materialize=%d", ev.count("warn", event.ReasonTokenRefreshFailed), mats.Load())
	}
	b, err := os.ReadFile(tokenPath)
	if err != nil || string(b) != "second" {
		t.Fatalf("host token %q err %v, refresh must still rewrite the file", b, err)
	}
	got, err := e.Get("default", "ref")
	if err != nil || got.Status.Phase != corev1.PodRunning {
		t.Fatalf("phase %v err %v, a refused copy retries until expiry", got.Status.Phase, err)
	}
	if slots.Used() != 1 {
		t.Fatalf("slots=%d, token retry must keep the VM", slots.Used())
	}
}
