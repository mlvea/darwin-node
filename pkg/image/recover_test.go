package image

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mlvea/darwin-node/internal/digest"
)

func TestRecoverRestoresCommittedImageAfterCrash(t *testing.T) {
	root := t.TempDir()
	ref := "example.com/macos:v1"
	key := CacheKey(ref)
	backup := filepath.Join(root, "image-replacing", key)
	plantCommitted(t, backup, "committed-disk")
	partial := filepath.Join(root, "image-partial", key)
	if err := os.MkdirAll(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "disk.img"), []byte("SHORT"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Recover(root); err != nil {
		t.Fatal(err)
	}
	img, err := LoadCommitted(CacheDir(root, ref))
	if err != nil {
		t.Fatalf("committed image was not restored: %v", err)
	}
	got, err := os.ReadFile(img.DiskPath)
	if err != nil || string(got) != "committed-disk" {
		t.Fatalf("restored disk %q err=%v", got, err)
	}
	if _, err := os.Stat(backup); !os.IsNotExist(err) {
		t.Fatalf("backup still present: %v", err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial still present: %v", err)
	}
}

func TestRecoverDropsUncommittedPartial(t *testing.T) {
	root := t.TempDir()
	key := CacheKey("example.com/macos:new")
	partial := filepath.Join(root, "image-partial", key)
	if err := os.MkdirAll(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(partial, "disk.img"), []byte("SHORT"), 0o600); err != nil {
		t.Fatal(err)
	}
	replacing := filepath.Join(root, "image-replacing", key)
	if err := os.MkdirAll(replacing, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(replacing, "disk.img"), []byte("NO-PROVENANCE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Recover(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial still present: %v", err)
	}
	if _, err := os.Stat(replacing); !os.IsNotExist(err) {
		t.Fatalf("uncommitted backup still present: %v", err)
	}
	if _, err := os.Stat(CacheDir(root, "example.com/macos:new")); !os.IsNotExist(err) {
		t.Fatalf("uncommitted image was published: %v", err)
	}
}

func TestRecoverKeepsCommittedFinal(t *testing.T) {
	root := t.TempDir()
	ref := "example.com/macos:v1"
	final := CacheDir(root, ref)
	plantCommitted(t, final, "published")
	plantCommitted(t, filepath.Join(root, "image-replacing", CacheKey(ref)), "previous")
	partial := filepath.Join(root, "image-partial", CacheKey(ref))
	if err := os.MkdirAll(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Recover(root); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(final, "disk.img"))
	if err != nil || string(got) != "published" {
		t.Fatalf("final disk %q err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(root, "image-replacing", CacheKey(ref))); !os.IsNotExist(err) {
		t.Fatalf("old backup kept beside a committed image: %v", err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial kept beside a committed image: %v", err)
	}
}

func TestRecoverDoesNotFollowImageSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(root, "image-partial")
	if err := os.MkdirAll(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(partial, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := Recover(root); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "keep" {
		t.Fatalf("recovery followed an image symlink: %q %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(partial, "escape")); !os.IsNotExist(err) {
		t.Fatalf("symlink left in image-partial: %v", err)
	}
}

func TestRecoverDoesNotReadThroughStagingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	victim := filepath.Join(outside, "keep-me")
	if err := os.MkdirAll(victim, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "disk.img"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "image-partial")); err != nil {
		t.Fatal(err)
	}
	if err := Recover(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(victim, "disk.img")); err != nil {
		t.Fatalf("recovery followed image-partial: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(root, "image-partial")); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Fatal("staging symlink still present")
	}
}

func TestRecoverDoesNotPromoteLegacyCacheKey(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "image-replacing", "team_macos--15")
	plantCommitted(t, legacy, "COLLIDED")
	if err := Recover(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy cache key was kept: %v", err)
	}
	img, err := LoadCommitted(CacheDir(root, "team/macos:15"))
	if err == nil {
		body, _ := os.ReadFile(img.DiskPath)
		if string(body) == "COLLIDED" {
			t.Fatal("legacy key was migrated onto team/macos:15")
		}
	}
	img, err = LoadCommitted(CacheDir(root, "team_macos:15"))
	if err == nil {
		body, _ := os.ReadFile(img.DiskPath)
		if string(body) == "COLLIDED" {
			t.Fatal("legacy key was migrated onto team_macos:15")
		}
	}
}

func plantCommitted(t *testing.T, dir, disk string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeTestImage(t, dir, disk, "AUX")
	diskSum, err := digest.FileSHA256(filepath.Join(dir, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	auxSum, err := digest.FileSHA256(filepath.Join(dir, "aux.img"))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteProvenance(dir, Provenance{
		Files: map[string]string{"disk.img": diskSum.String(), "aux.img": auxSum.String()},
	}); err != nil {
		t.Fatal(err)
	}
}
