package image

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeImageDirWithDisk writes a minimal valid image dir whose disk.img is
// data. LoadDir needs no digests; verification is optional.
func writeImageDirWithDisk(t *testing.T, dir string, data []byte) {
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
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "aux.img"), []byte("AUX"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func patternDisk(size int, seed byte) []byte {
	disk := make([]byte, size)
	for i := range disk {
		disk[i] = seed + byte(i%251)
	}
	return disk
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return b
}

func TestDeltaRoundTrip(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base")
	deltaDir := filepath.Join(t.TempDir(), "delta")
	dest := filepath.Join(t.TempDir(), "dest")

	baseDisk := patternDisk(10<<20, 1)
	writeImageDirWithDisk(t, base, baseDisk)

	// Target: two changed regions plus growth beyond the base size.
	mut := randomBytes(32)
	targetDisk := append([]byte(nil), baseDisk...)
	copy(targetDisk[3<<20:], mut)
	targetDisk = append(targetDisk, mut...)
	target := filepath.Join(t.TempDir(), "target")
	writeImageDirWithDisk(t, target, targetDisk)

	ref, err := CreatePatch(
		filepath.Join(base, "disk.img"),
		filepath.Join(target, "disk.img"),
		deltaDir, "disk.img")
	if err != nil {
		t.Fatal(err)
	}
	patchInfo, err := os.Stat(filepath.Join(deltaDir, "disk.img.patch"))
	if err != nil || patchInfo.Size() == 0 {
		t.Fatalf("patch missing or empty: %v", err)
	}
	if patchInfo.Size() >= int64(len(targetDisk)) {
		t.Fatalf("patch not smaller than full disk: %d", patchInfo.Size())
	}
	if ref.DestSize != int64(len(targetDisk)) {
		t.Fatalf("dest size %d want %d", ref.DestSize, len(targetDisk))
	}

	if err := ApplyDelta(base, deltaDir, dest); err != nil {
		t.Fatal(err)
	}
	gotSum, err := fileSHA256(filepath.Join(dest, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	wantSum, _ := hex.DecodeString(ref.DestSHA[len("sha256-"):])
	if hex.EncodeToString(gotSum) != hex.EncodeToString(wantSum) {
		t.Fatal("applied disk does not match target")
	}
	// The destination must be a loadable image dir.
	if _, err := LoadDir(dest); err != nil {
		t.Fatalf("applied dir not a valid image: %v", err)
	}

	// Applying twice must refuse: destination exists.
	if err := ApplyDelta(base, deltaDir, dest); err == nil {
		t.Fatal("second apply should fail")
	}
}

func TestDeltaRejectsWrongBase(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base")
	target := filepath.Join(t.TempDir(), "target")
	dest := filepath.Join(t.TempDir(), "dest")
	deltaDir := filepath.Join(t.TempDir(), "delta")

	writeImageDirWithDisk(t, base, patternDisk(8<<20, 7))
	writeImageDirWithDisk(t, target, patternDisk(8<<20, 9))

	if _, err := CreatePatch(
		filepath.Join(base, "disk.img"),
		filepath.Join(target, "disk.img"),
		deltaDir, "disk.img"); err != nil {
		t.Fatal(err)
	}
	// A different base with the same layout must be rejected by digest.
	other := filepath.Join(t.TempDir(), "other")
	writeImageDirWithDisk(t, other, patternDisk(8<<20, 42))
	if err := ApplyDelta(other, deltaDir, dest); err == nil {
		t.Fatal("wrong base must be rejected")
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("failed apply must not leave destination: %v", err)
	}
}

func TestDeltaShrinkAndIdenticalBase(t *testing.T) {
	base := filepath.Join(t.TempDir(), "base")
	deltaDir := filepath.Join(t.TempDir(), "delta")
	fakeDisk := patternDisk(6<<20, 3)
	writeImageDirWithDisk(t, base, fakeDisk)

	// Identical content: patch has zero records and identical digest.
	same := filepath.Join(t.TempDir(), "same")
	writeImageDirWithDisk(t, same, fakeDisk)
	refSame, err := CreatePatch(
		filepath.Join(base, "disk.img"),
		filepath.Join(same, "disk.img"),
		deltaDir, "same.img")
	if err != nil {
		t.Fatal(err)
	}
	if refSame.DestSHA != refSame.BaseSHA {
		t.Fatal("identical files must share digest")
	}

	// Shrunk content: apply truncates to DestSize.
	shrunk := fakeDisk[:4<<20]
	shrinkDir := filepath.Join(t.TempDir(), "shrink")
	writeImageDirWithDisk(t, shrinkDir, shrunk)
	shrinkDelta := filepath.Join(t.TempDir(), "delta-shrink")
	refShrink, err := CreatePatch(
		filepath.Join(base, "disk.img"),
		filepath.Join(shrinkDir, "disk.img"),
		shrinkDelta, "disk.img")
	if err != nil {
		t.Fatal(err)
	}
	if refShrink.DestSize != int64(len(shrunk)) {
		t.Fatalf("dest size %d want %d", refShrink.DestSize, len(shrunk))
	}

	dest := filepath.Join(t.TempDir(), "dest")
	if err := ApplyDelta(base, shrinkDelta, dest); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dest, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(shrunk)) {
		t.Fatalf("size %d want %d", info.Size(), len(shrunk))
	}
	sum, err := fileSHA256(filepath.Join(dest, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := hex.DecodeString(refShrink.DestSHA[len("sha256-"):])
	if hex.EncodeToString(sum) != hex.EncodeToString(want) {
		t.Fatal(fmt.Sprintf("shrunk disk mismatch"))
	}
}
