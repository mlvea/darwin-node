package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/internal/leakcheck"
	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/volume"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func contractEngine(t *testing.T) (*Engine, *capacity.Slots, *fake.Runtime, config.Config) {
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
	rt := fake.New()
	e := New(cfg, slots, rt, sidecar.None{}, event.Nop{}, "10.0.0.1")
	return e, slots, rt, cfg
}

func TestEmptyDirSizeLimitFailsPodAndFreesSlot(t *testing.T) {
	e, slots, _, cfg := contractEngine(t)
	limit := resource.MustParse("4")
	pod := samplePod("ed", "uid-ed")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "ed", corev1.PodRunning)
	dir := filepath.Join(cfg.CacheDir, "pods", "uid-ed", "volumes", "scratch")
	if err := os.WriteFile(filepath.Join(dir, "small"), []byte("ab"), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	got, err := e.Get("default", "ed")
	if err != nil || got.Status.Phase != corev1.PodRunning {
		t.Fatalf("under limit: phase %v err %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "big"), []byte("0123456789"), 0o644); err != nil {
		t.Fatal(err)
	}
	failed := waitPhase(t, e, "default", "ed", corev1.PodFailed)
	if !strings.Contains(failed.Status.Message, "sizeLimit") {
		t.Fatalf("message %q", failed.Status.Message)
	}
	if slots.Used() != 0 {
		t.Fatalf("slot not released: %d", slots.Used())
	}
	if err := e.Create(context.Background(), samplePod("next", "uid-next"), Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "next", corev1.PodRunning)
	_ = e.Delete(context.Background(), "default", "next", 0)
}

func TestEmptyDirSymlinkEscapeFailsPod(t *testing.T) {
	e, slots, _, cfg := contractEngine(t)
	limit := resource.MustParse("1Mi")
	pod := samplePod("link", "uid-link")
	pod.Spec.Volumes = []corev1.Volume{{
		Name:         "scratch",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &limit}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "link", corev1.PodRunning)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(cfg.CacheDir, "pods", "uid-link", "volumes", "scratch")
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	failed := waitPhase(t, e, "default", "link", corev1.PodFailed)
	if !strings.Contains(failed.Status.Message, "escape") && !strings.Contains(failed.Status.Message, "symlink") {
		t.Fatalf("message %q", failed.Status.Message)
	}
	if slots.Used() != 0 {
		t.Fatalf("slot not released: %d", slots.Used())
	}
}

func TestProjectedTokenMissingFailsClosed(t *testing.T) {
	e, slots, _, cfg := contractEngine(t)
	pod := samplePod("tok", "uid-tok")
	pod.Spec.ServiceAccountName = "builder"
	pod.Spec.Volumes = []corev1.Volume{{
		Name: "proj",
		VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
			{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token", Audience: "api"}},
		}}},
	}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name: "proj", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount",
	}}
	if err := e.Create(context.Background(), pod, Credentials{}); err != nil {
		t.Fatal(err)
	}
	failed := waitPhase(t, e, "default", "tok", corev1.PodFailed)
	if !strings.Contains(failed.Status.Message, "token") {
		t.Fatalf("message %q", failed.Status.Message)
	}
	if _, err := os.Stat(filepath.Join(cfg.CacheDir, "pods", "uid-tok", "volumes", "proj", "token")); !os.IsNotExist(err) {
		t.Fatal("empty token file must not be written")
	}
	if slots.Used() != 0 {
		t.Fatalf("slot not released: %d", slots.Used())
	}
}

func TestProjectedTokenRefreshRewritesAndRematerializes(t *testing.T) {
	e, _, rt, cfg := contractEngine(t)
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
		ExpiresAt: time.Now().Add(120 * time.Millisecond),
	}}}
	if err := e.Create(context.Background(), pod, creds); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, e, "default", "ref", corev1.PodRunning)
	tokenPath := filepath.Join(cfg.CacheDir, "pods", "uid-ref", "volumes", "proj", "token")
	b, err := os.ReadFile(tokenPath)
	if err != nil || string(b) != "first" {
		t.Fatalf("initial token %q %v", b, err)
	}
	before := rt.MaterializeCalls()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, err = os.ReadFile(tokenPath)
		if err == nil && string(b) == "second" && rt.MaterializeCalls() > before {
			_ = e.Delete(context.Background(), "default", "ref", 0)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("token %q materialize %d -> %d err %v", b, before, rt.MaterializeCalls(), err)
}

func TestProjectedTokenRefreshFailureAfterExpiryFailsPod(t *testing.T) {
	e, slots, _, cfg := contractEngine(t)
	tokenPath := filepath.Join(cfg.CacheDir, "pods", "uid-exp", "volumes", "proj", "token")
	var saw string
	e.SetTokenMinter(func(context.Context, *corev1.Pod) ([]volume.ProjectedToken, error) {
		b, _ := os.ReadFile(tokenPath)
		saw = string(b)
		return nil, errors.New("mint down")
	})
	pod := samplePod("exp", "uid-exp")
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
		ExpiresAt: time.Now().Add(80 * time.Millisecond),
	}}}
	if err := e.Create(context.Background(), pod, creds); err != nil {
		t.Fatal(err)
	}
	failed := waitPhase(t, e, "default", "exp", corev1.PodFailed)
	if !strings.Contains(failed.Status.Message, "mint down") {
		t.Fatalf("message %q", failed.Status.Message)
	}
	if saw != "first" {
		t.Fatalf("refresh must not blank the last good token, saw %q", saw)
	}
	if slots.Used() != 0 {
		t.Fatalf("slot not released: %d", slots.Used())
	}
}
