package nodecert

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareDirCreatesPrivateDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	if err := PrepareDir(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %o, want 0700", fi.Mode().Perm())
	}
}

func TestPrepareDirTightensGroupReadable(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "kubelet-server-1.pem")
	if err := os.WriteFile(key, []byte("key"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDir(dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %o, want 0700", fi.Mode().Perm())
	}
	kf, err := os.Stat(key)
	if err != nil {
		t.Fatal(err)
	}
	if kf.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o, want 0600", kf.Mode().Perm())
	}
}

func TestPrepareDirRefusesGroupOrWorldWritable(t *testing.T) {
	for _, mode := range []os.FileMode{0o777, 0o775, 0o707, 0o722} {
		dir := filepath.Join(t.TempDir(), "pki")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		err := PrepareDir(dir)
		if err == nil {
			t.Fatalf("mode %o was accepted", mode)
		}
		fi, stErr := os.Stat(dir)
		if stErr != nil {
			t.Fatal(stErr)
		}
		if fi.Mode().Perm() != mode {
			t.Fatalf("mode %o changed to %o after refusal", mode, fi.Mode().Perm())
		}
	}
}

func TestPrepareDirRefusesSymlink(t *testing.T) {
	parent := t.TempDir()
	real := filepath.Join(parent, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDir(link); err == nil {
		t.Fatal("symlink cert dir was accepted")
	}
}

func TestPrepareDirRefusesSymlinkOutside(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "kubelet-server-current.pem")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDir(dir); err == nil {
		t.Fatal("symlink pointing outside the cert dir was accepted")
	}

	rel := filepath.Join(dir, "escape.pem")
	if err := os.Symlink("../outside", rel); err != nil {
		t.Fatal(err)
	}
	// The absolute link already failed. Replace it so the relative one is the only escape.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDir(dir); err == nil {
		t.Fatal("relative symlink pointing outside the cert dir was accepted")
	}
}

func TestPrepareDirAllowsSymlinkInside(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "kubelet-server-1.pem")
	if err := os.WriteFile(target, []byte("pem"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "kubelet-server-current.pem")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDir(dir); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareDirRefusesEmptyAndFile(t *testing.T) {
	if err := PrepareDir("  "); err == nil {
		t.Fatal("empty path")
	}
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareDir(file); err == nil {
		t.Fatal("file path was accepted")
	}
}
