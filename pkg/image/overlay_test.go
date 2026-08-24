package image

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCloneOverlay(t *testing.T) {
	src := t.TempDir()
	disk := filepath.Join(src, "disk.img")
	aux := filepath.Join(src, "aux.img")
	if err := os.WriteFile(disk, []byte("DISK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(aux, []byte("AUX"), 0o644); err != nil {
		t.Fatal(err)
	}
	ov, err := CloneOverlay(LocalImage{DiskPath: disk, AuxPath: aux}, filepath.Join(t.TempDir(), "ov"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(ov.DiskPath)
	if string(b) != "DISK" {
		t.Fatalf("disk %q", b)
	}
	if err := ov.Remove(); err != nil {
		t.Fatal(err)
	}
}
