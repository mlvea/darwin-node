package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mlvea/darwin-node/internal/digest"
)

func TestCacheKeySeparatesSlashFromUnderscore(t *testing.T) {
	a := CacheKey("team/macos:15")
	b := CacheKey("team_macos:15")
	if a == b {
		t.Fatalf("team/macos:15 and team_macos:15 share cache key %q", a)
	}
	if strings.ContainsAny(a, "/:") {
		t.Fatalf("cache key %q is not one path segment", a)
	}
}

func TestCacheKeyDoesNotSplitHostPort(t *testing.T) {
	a := CacheKey("127.0.0.1:5000/macos:latest")
	if a == CacheKey("127.0.0.1:5001/macos:latest") {
		t.Fatal("host port was dropped from the cache key")
	}
	if CacheKey("ghcr.io/org/macos:15") == CacheKey("ghcr.io/org/macos:16") {
		t.Fatal("tag was dropped from the cache key")
	}
	if len(a) != 64 || strings.ContainsAny(a, "/:_") {
		t.Fatalf("key %q is not one hex path segment", a)
	}
}

func TestLoadDir(t *testing.T) {
	dir := t.TempDir()
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
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte("DISK"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux.img"), []byte("AUX"), 0o644); err != nil {
		t.Fatal(err)
	}
	img, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if img.Config.HardwareModelData != "abc" || img.DiskPath == "" {
		t.Fatalf("%+v", img)
	}
	if img.DiskDig != "" || img.AuxDig != "" {
		t.Fatalf("no provenance: %+v", img)
	}
}

func writeTestImage(t *testing.T, dir, disk, aux string) {
	t.Helper()
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
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte(disk), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux.img"), []byte(aux), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirUsesProvenanceDigests(t *testing.T) {
	dir := t.TempDir()
	writeTestImage(t, dir, "DISK", "AUX")
	diskSum, err := digest.FileSHA256(filepath.Join(dir, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	auxSum, err := digest.FileSHA256(filepath.Join(dir, "aux.img"))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteProvenance(dir, Provenance{
		Files: map[string]string{
			"disk.img": diskSum.String(),
			"aux.img":  auxSum.String(),
		},
		Sizes: map[string]int64{"disk.img": 4, "aux.img": 3},
	}); err != nil {
		t.Fatal(err)
	}
	img, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if img.DiskDig != diskSum || img.AuxDig != auxSum {
		t.Fatalf("digests disk=%s aux=%s", img.DiskDig, img.AuxDig)
	}
	if err := img.VerifyOptional(); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyOptionalTamperFailsAgainstProvenance(t *testing.T) {
	dir := t.TempDir()
	writeTestImage(t, dir, "DISK", "AUX")
	diskPath := filepath.Join(dir, "disk.img")
	original, err := digest.FileSHA256(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	auxSum, err := digest.FileSHA256(filepath.Join(dir, "aux.img"))
	if err != nil {
		t.Fatal(err)
	}
	if err := digest.WriteSidecar(diskPath, original); err != nil {
		t.Fatal(err)
	}
	if err := WriteProvenance(dir, Provenance{
		Files: map[string]string{"disk.img": original.String(), "aux.img": auxSum.String()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(diskPath, []byte("HACK"), 0o644); err != nil {
		t.Fatal(err)
	}
	newSum, err := digest.FileSHA256(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := digest.WriteSidecar(diskPath, newSum); err != nil {
		t.Fatal(err)
	}
	img, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := img.VerifyOptional(); err == nil {
		t.Fatal("tampered disk with regenerated sidecar must fail against provenance")
	}
}

func TestLoadDirRejectsSymlinkToAnotherDisk(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "disk.img"), []byte("OTHER-POD"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeTestImage(t, dir, "OWN", "AUX")
	if err := os.Remove(filepath.Join(dir, "disk.img")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "disk.img"), filepath.Join(dir, "disk.img")); err != nil {
		t.Fatal(err)
	}
	img, err := LoadDir(dir)
	if err == nil {
		t.Fatalf("symlinked disk was loaded from %s", img.DiskPath)
	}
}

func TestLoadDirRejectsStoragePathEscape(t *testing.T) {
	dir := t.TempDir()
	cfg := `{
		"os":"darwin",
		"hardwareModelData":"abc",
		"storage":[{"mediatype":"application/vnd.darwin-node.disk.v1","file":"../disk.img"}]
	}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadDir(dir); err == nil {
		t.Fatal("storage file ../disk.img must be rejected")
	}
}

func TestVerifyOptionalSkipsLocalBakeWithoutSidecar(t *testing.T) {
	dir := t.TempDir()
	writeTestImage(t, dir, "DISK", "AUX")
	img, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := img.VerifyOptional(); err != nil {
		t.Fatal(err)
	}
}
