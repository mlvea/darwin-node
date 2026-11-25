package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/mlvea/darwin-node/pkg/image"

	"github.com/spf13/cobra"
)

func TestDeltaCreateTwiceThenApply(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "base")
	target := filepath.Join(root, "target")
	deltaDir := filepath.Join(root, "delta")
	out := filepath.Join(root, "out")

	baseDisk := bytes.Repeat([]byte{1}, 64*1024)
	targetDisk := append([]byte(nil), baseDisk...)
	targetDisk[100] = 2
	targetDisk = append(targetDisk, 3, 3, 3)
	writeCLIImage(t, base, baseDisk)
	writeCLIImage(t, target, targetDisk)

	if err := runImageCmd(t, cmdDeltaCreate(), "--base", base, "--target", target, "--out", deltaDir); err != nil {
		t.Fatal(err)
	}
	if err := runImageCmd(t, cmdDeltaCreate(), "--base", base, "--target", target, "--out", deltaDir); err != nil {
		t.Fatal(err)
	}
	assertOneDiskPatch(t, deltaDir)

	if err := runImageCmd(t, cmdDeltaApply(), "--base", base, "--delta", deltaDir, "--out", out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(out, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, targetDisk) {
		t.Fatalf("applied disk %d bytes does not match target %d bytes", len(got), len(targetDisk))
	}

	// A later create for the same file name replaces the from/to pair.
	// delta-apply must produce the new target, still from one manifest entry.
	revised := append([]byte(nil), baseDisk...)
	revised[200] = 9
	revised = revised[:len(revised)-100]
	revisedDir := filepath.Join(root, "revised")
	writeCLIImage(t, revisedDir, revised)
	if err := runImageCmd(t, cmdDeltaCreate(), "--base", base, "--target", revisedDir, "--out", deltaDir); err != nil {
		t.Fatal(err)
	}
	assertOneDiskPatch(t, deltaDir)
	revisedOut := filepath.Join(root, "revised-out")
	if err := runImageCmd(t, cmdDeltaApply(), "--base", base, "--delta", deltaDir, "--out", revisedOut); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(revisedOut, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, revised) {
		t.Fatalf("replaced delta applied %d bytes, target is %d", len(got), len(revised))
	}
}

func TestDeltaCreateCollapsesAnExistingDuplicate(t *testing.T) {
	root := t.TempDir()
	base := filepath.Join(root, "base")
	target := filepath.Join(root, "target")
	deltaDir := filepath.Join(root, "delta")
	out := filepath.Join(root, "out")
	baseDisk := bytes.Repeat([]byte{4}, 32*1024)
	targetDisk := append([]byte(nil), baseDisk...)
	targetDisk[10] = 7
	writeCLIImage(t, base, baseDisk)
	writeCLIImage(t, target, targetDisk)

	if err := runImageCmd(t, cmdDeltaCreate(), "--base", base, "--target", target, "--out", deltaDir); err != nil {
		t.Fatal(err)
	}
	man, err := image.ReadDeltaManifest(deltaDir)
	if err != nil {
		t.Fatal(err)
	}
	man.Patches = append(man.Patches, man.Patches...)
	if err := image.WriteDeltaManifest(deltaDir, man); err != nil {
		t.Fatal(err)
	}
	if err := runImageCmd(t, cmdDeltaCreate(), "--base", base, "--target", target, "--out", deltaDir); err != nil {
		t.Fatal(err)
	}
	assertOneDiskPatch(t, deltaDir)
	if err := runImageCmd(t, cmdDeltaApply(), "--base", base, "--delta", deltaDir, "--out", out); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(out, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, targetDisk) {
		t.Fatalf("applied %d bytes, target %d", len(got), len(targetDisk))
	}
}

func assertOneDiskPatch(t *testing.T, deltaDir string) {
	t.Helper()
	man, err := image.ReadDeltaManifest(deltaDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(man.Patches) != 1 || man.Patches[0].Name != "disk.img" {
		t.Fatalf("manifest patches = %+v, want one disk.img entry", man.Patches)
	}
}

func runImageCmd(t *testing.T, c *cobra.Command, args ...string) error {
	t.Helper()
	c.SetArgs(args)
	c.SetOut(io.Discard)
	c.SetErr(io.Discard)
	c.SilenceUsage = true
	c.SilenceErrors = true
	return c.Execute()
}

func writeCLIImage(t *testing.T, dir string, disk []byte) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := `{
		"os":"darwin",
		"hardwareModelData":"abc",
		"machineIdData":"def",
		"storage":[
			{"mediatype":"application/vnd.darwin-node.disk.v1","file":"disk.img"},
			{"mediatype":"application/vnd.darwin-node.aux.v1","file":"aux.img"}
		]
	}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), disk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux.img"), []byte("AUX"), 0o644); err != nil {
		t.Fatal(err)
	}
}
