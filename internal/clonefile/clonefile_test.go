package clonefile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileDoesNotCloneThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "other-pod-disk")
	if err := os.WriteFile(secret, []byte("OTHER-POD"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "disk.img")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "overlay", "disk.img")
	err := File(link, dst)
	if fi, lerr := os.Lstat(dst); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("clone copied the symlink into the overlay")
	}
	if b, rerr := os.ReadFile(dst); rerr == nil && string(b) == "OTHER-POD" {
		t.Fatal("clone followed a symlink and copied another pod's disk")
	}
	if err == nil {
		t.Fatal("symlink source was cloned")
	}
}

func TestFileRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("hello-clone"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := File(src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello-clone" {
		t.Fatalf("got %q", b)
	}
}
