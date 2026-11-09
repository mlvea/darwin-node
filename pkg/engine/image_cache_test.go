package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/darwin-node/darwin-node/pkg/image"

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
