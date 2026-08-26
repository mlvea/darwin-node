package image

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProbeProvAfterDeltaPull(t *testing.T) {
	reg := newCASRegistry()
	defer reg.close()
	baseDisk := patternDisk(2<<20, 5)
	baseRef, _ := publishBase(t, reg, "p/base", "v1", baseDisk)
	targetDisk := append([]byte(nil), baseDisk...)
	targetDisk[100] ^= 0xff
	deltaDir := t.TempDir()
	CreatePatchSilent(t, baseDisk, targetDisk, deltaDir)
	patch, _ := os.ReadFile(filepath.Join(deltaDir, "disk.img.patch"))
	deltaRef := reg.refFor("p/app", "v2")
	pushArtifact(t, reg, deltaRef, testConfigJSON(t, "x"), []layerSpec{{
		mediaType: MediaTypeDiskDelta, title: "disk.delta.patch", data: patch,
		ann: map[string]string{
			AnnotDeltaBaseRef:       baseRef,
			AnnotDeltaBaseDigest:    digestOf(baseDisk),
			AnnotUncompressedDigest: "sha256:" + digestOf(targetDisk),
			AnnotUncompressedSize:   "2097152",
		},
	}})
	m := NewManager(t.TempDir())
	baseImg, err := m.Pull(context.Background(), baseRef, RegistryCreds{}, false)
	if err != nil {
		t.Fatal(err)
	}
	bb, _ := os.ReadFile(filepath.Join(baseImg.Dir, "provenance.json"))
	t.Logf("BASE prov=%s", string(bb))
	img, err := m.Pull(context.Background(), deltaRef, RegistryCreds{}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("DiskDig=%s want=%s", img.DiskDig, digestOf(targetDisk))
	b, _ := os.ReadFile(filepath.Join(img.Dir, "provenance.json"))
	t.Logf("prov=%s", string(b))
	actual, _ := os.ReadFile(img.DiskPath)
	t.Logf("actual-disk-sha=%s len=%d eqBase=%v eqTarget=%v", digestOf(actual), len(actual),
		bytes.Equal(actual, baseDisk), bytes.Equal(actual, targetDisk))
	entries, _ := os.ReadDir(img.Dir)
	for _, e := range entries {
		t.Logf("file %s", e.Name())
	}
}
