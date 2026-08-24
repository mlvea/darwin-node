package image

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPackDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte("d"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux.img"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PackDir(dir, Config{HardwareModelData: "hw", MachineIdData: "id"}); err != nil {
		t.Fatal(err)
	}
	img, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if img.Config.HardwareModelData != "hw" {
		t.Fatalf("%+v", img.Config)
	}
}

// Issue 007: pack must refuse configs that verify/LoadDir would reject.
func TestPackDirRequiresHardwareModel(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte("d"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux.img"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PackDir(dir, Config{}); err == nil {
		t.Fatal("expected error for empty hardwareModelData")
	}
	if _, err := os.Stat(filepath.Join(dir, "config.json")); err == nil {
		t.Fatal("pack must not write config.json when hardwareModelData is missing")
	}
}
