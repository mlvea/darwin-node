package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mlvea/darwin-node/internal/digest"
	"github.com/mlvea/darwin-node/pkg/image"

	corev1 "k8s.io/api/core/v1"
)

func writeBake(t *testing.T, dir, disk string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfg := `{
		"os":"darwin",
		"hardwareModelData":"abc",
		"storage":[
			{"mediatype":"application/vnd.darwin-node.disk.v1","file":"disk.img"},
			{"mediatype":"application/vnd.darwin-node.aux.v1","file":"aux.img"}
		]
	}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte(disk), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux.img"), []byte("AUX"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A registry cache without provenance.json is an unfinished pull (the
// process died after the blobs were copied, or a short write left a
// truncated disk). It must not be cloned into a pod. A local bake, which
// is a directory path and not the pull cache, still loads.
func TestResolveImageRejectsUncommittedPullCache(t *testing.T) {
	e := testEngine(t)
	ref := "127.0.0.1:1/darwin/macos:partial"
	writeBake(t, image.CacheDir(e.cfg.CacheDir, ref), "SHORT")
	disk, _, hw, err := e.resolveImage(context.Background(), ref, "uid-partial", corev1.PullNever, Credentials{})
	if err == nil {
		body, _ := os.ReadFile(disk)
		t.Fatalf("uncommitted pull cache was used, hardware %q disk %q", hw, body)
	}
}

func TestResolveImageAcceptsLocalBakeWithoutProvenance(t *testing.T) {
	e := testEngine(t)
	dir := t.TempDir()
	writeBake(t, dir, "BAKE")
	disk, _, hw, err := e.resolveImage(context.Background(), dir, "uid-bake", corev1.PullNever, Credentials{})
	if err != nil {
		t.Fatal(err)
	}
	if hw != "abc" {
		t.Fatalf("hardware %q", hw)
	}
	body, err := os.ReadFile(disk)
	if err != nil || string(body) != "BAKE" {
		t.Fatalf("disk %q err=%v", body, err)
	}
}

func writeCommittedBake(t *testing.T, dir, disk string) {
	t.Helper()
	writeBake(t, dir, disk)
	diskSum, err := digest.FileSHA256(filepath.Join(dir, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	auxSum, err := digest.FileSHA256(filepath.Join(dir, "aux.img"))
	if err != nil {
		t.Fatal(err)
	}
	if err := image.WriteProvenance(dir, image.Provenance{
		Files: map[string]string{"disk.img": diskSum.String(), "aux.img": auxSum.String()},
	}); err != nil {
		t.Fatal(err)
	}
}

type scriptedPuller struct {
	calls  int
	ignore bool
	err    error
}

func (s *scriptedPuller) Pull(_ context.Context, _ string, _ image.RegistryCreds, ignoreCache bool) (image.LocalImage, error) {
	s.calls++
	s.ignore = ignoreCache
	if s.err != nil {
		return image.LocalImage{}, s.err
	}
	return image.LocalImage{}, errors.New("puller returned no image")
}

func TestResolveImageRejectsHostPathInClusterMode(t *testing.T) {
	e := testEngine(t)
	e.cfg.Cluster = true
	dir := t.TempDir()
	writeBake(t, dir, "PRIVATE-IMAGE")
	disk, _, _, err := e.resolveImage(context.Background(), dir, "uid-attack", corev1.PullNever, Credentials{})
	if err == nil {
		body, _ := os.ReadFile(disk)
		t.Fatalf("cluster mode cloned host directory %s disk %q", dir, body)
	}
	overlay := filepath.Join(e.cfg.CacheDir, "pods", "uid-attack", "overlay", "disk.img")
	if b, rerr := os.ReadFile(overlay); rerr == nil && string(b) == "PRIVATE-IMAGE" {
		t.Fatal("overlay contains the host directory image")
	}

	ref := "ghcr.io/secret/macos:15"
	cache := image.CacheDir(e.cfg.CacheDir, ref)
	writeBake(t, cache, "SECRET-DISK")
	disk, _, _, err = e.resolveImage(context.Background(), cache, "uid-cache", corev1.PullNever, Credentials{})
	if err == nil {
		body, _ := os.ReadFile(disk)
		t.Fatalf("pod image was a cache directory and booted %q", body)
	}
}

func TestResolveImageAlwaysPullsCachedTag(t *testing.T) {
	e := testEngine(t)
	ref := "127.0.0.1:1/darwin/macos:moving"
	writeCommittedBake(t, image.CacheDir(e.cfg.CacheDir, ref), "STALE")
	p := &scriptedPuller{err: errors.New("tag moved")}
	e.puller = p
	_, _, _, err := e.resolveImage(context.Background(), ref, "uid-always", corev1.PullAlways, Credentials{})
	if p.calls == 0 || !p.ignore {
		t.Fatalf("Always used the cached tag without pulling: calls=%d ignore=%v err=%v", p.calls, p.ignore, err)
	}
}

func TestResolveImageDoesNotReuseCacheWithoutPuller(t *testing.T) {
	e := testEngine(t)
	ref := "127.0.0.1:1/darwin/macos:private"
	writeCommittedBake(t, image.CacheDir(e.cfg.CacheDir, ref), "PRIVATE")
	p := &scriptedPuller{err: errors.New("unauthorized")}
	e.puller = p
	disk, _, _, err := e.resolveImage(context.Background(), ref, "uid-cache", corev1.PullIfNotPresent, Credentials{})
	if err == nil {
		body, _ := os.ReadFile(disk)
		t.Fatalf("cached image used without asking the puller, disk %q", body)
	}
	if p.calls == 0 {
		t.Fatal("cache hit skipped the puller")
	}
}

func TestResolveImageNeverStillRequiresAuthorization(t *testing.T) {
	e := testEngine(t)
	ref := "127.0.0.1:1/darwin/macos:never"
	writeCommittedBake(t, image.CacheDir(e.cfg.CacheDir, ref), "PRIVATE")
	p := &scriptedPuller{err: errors.New("unauthorized")}
	e.puller = p
	disk, _, _, err := e.resolveImage(context.Background(), ref, "uid-never", corev1.PullNever, Credentials{})
	if err == nil {
		body, _ := os.ReadFile(disk)
		t.Fatalf("PullNever booted a cached image the puller would not authorize, disk %q calls=%d", body, p.calls)
	}
}
