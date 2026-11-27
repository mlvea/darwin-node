package vmidentity_test

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mlvea/darwin-node/pkg/vmidentity"
)

const bakedMachineID = "baked-in-static-id"

var goldenAuxBytes = []byte("GOLDEN-NVRAM-LOCALPOLICY-v1")

func TestColdBootPerPodIdentityAndAux(t *testing.T) {
	golden := writeGoldenAux(t, goldenAuxBytes)
	root := t.TempDir()

	first := coldBoot(t, golden, root, "pod-seq-0", 0)
	second := coldBoot(t, golden, root, "pod-seq-1", 1)
	assertFreshIdentity(t, first)
	assertFreshIdentity(t, second)
	if first.MachineID == second.MachineID {
		t.Fatalf("sequential pods share machine id %q", first.MachineID)
	}
	assertPrivateAux(t, golden, first, goldenAuxBytes)
	assertPrivateAux(t, golden, second, goldenAuxBytes)
	if first.AuxPath == second.AuxPath {
		t.Fatalf("sequential pods share aux %s", first.AuxPath)
	}

	var wg sync.WaitGroup
	concurrent := make([]vmidentity.Boot, 2)
	errs := make([]error, 2)
	for slot := 0; slot < 2; slot++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			concurrent[slot], errs[slot] = vmidentity.ColdBoot(vmidentity.Image{
				MachineIdentifierData: bakedMachineID,
				GoldenAux:             golden,
			}, vmidentity.Pod{
				UID:  fmt.Sprintf("pod-conc-%d", slot),
				Slot: slot,
				Dir:  filepath.Join(root, fmt.Sprintf("conc-%d", slot)),
			}, nil)
		}(slot)
	}
	wg.Wait()
	for slot := range concurrent {
		if errs[slot] != nil {
			t.Fatalf("slot %d: %v", slot, errs[slot])
		}
		assertFreshIdentity(t, concurrent[slot])
		assertPrivateAux(t, golden, concurrent[slot], goldenAuxBytes)
	}
	if concurrent[0].MachineID == concurrent[1].MachineID {
		t.Fatalf("concurrent slots share machine id %q", concurrent[0].MachineID)
	}
	if concurrent[0].AuxPath == concurrent[1].AuxPath {
		t.Fatalf("concurrent slots share aux %s", concurrent[0].AuxPath)
	}
	seen := map[string]string{
		first.MachineID:         first.UID,
		second.MachineID:        second.UID,
		concurrent[0].MachineID: concurrent[0].UID,
		concurrent[1].MachineID: concurrent[1].UID,
	}
	if len(seen) != 4 {
		t.Fatalf("machine ids collided: %+v", seen)
	}

	mutated := []byte("POD0-NVRAM-CHANGED")
	if err := os.WriteFile(concurrent[0].AuxPath, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, golden); !bytes.Equal(got, goldenAuxBytes) {
		t.Fatalf("golden aux changed to %q", got)
	}
	if got := mustRead(t, concurrent[1].AuxPath); !bytes.Equal(got, goldenAuxBytes) {
		t.Fatalf("slot 1 aux changed to %q", got)
	}
	if got := mustRead(t, first.AuxPath); !bytes.Equal(got, goldenAuxBytes) {
		t.Fatalf("sequential aux changed to %q", got)
	}
	if got := mustRead(t, concurrent[0].AuxPath); !bytes.Equal(got, mutated) {
		t.Fatalf("slot 0 aux = %q, want the write", got)
	}
}

func TestColdBootRejectsBakedMinterWithoutCreatingAux(t *testing.T) {
	golden := writeGoldenAux(t, goldenAuxBytes)
	dir := filepath.Join(t.TempDir(), "pod")
	_, err := vmidentity.ColdBoot(vmidentity.Image{
		MachineIdentifierData: bakedMachineID,
		GoldenAux:             golden,
	}, vmidentity.Pod{UID: "pod-baked", Slot: 0, Dir: dir}, func() (string, error) {
		return bakedMachineID, nil
	})
	if !errors.Is(err, vmidentity.ErrBakedMachineID) {
		t.Fatalf("err %v, want ErrBakedMachineID", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ephemeral")); !os.IsNotExist(statErr) {
		t.Fatalf("failed cold boot left an aux tree: %v", statErr)
	}
}

func TestColdBootMissingGoldenDoesNotCreateBlankAux(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pod")
	missing := filepath.Join(t.TempDir(), "no-such-aux.img")
	_, err := vmidentity.ColdBoot(vmidentity.Image{
		MachineIdentifierData: bakedMachineID,
		GoldenAux:             missing,
	}, vmidentity.Pod{UID: "pod-missing", Slot: 1, Dir: dir}, nil)
	if !errors.Is(err, vmidentity.ErrGoldenAux) {
		t.Fatalf("err %v, want ErrGoldenAux", err)
	}
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		t.Errorf("created %s while golden aux was missing", path)
		return nil
	}); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestColdBootBlankGoldenDoesNotCreateAux(t *testing.T) {
	golden := writeGoldenAux(t, nil)
	dir := filepath.Join(t.TempDir(), "pod")
	_, err := vmidentity.ColdBoot(vmidentity.Image{
		MachineIdentifierData: bakedMachineID,
		GoldenAux:             golden,
	}, vmidentity.Pod{UID: "pod-blank", Slot: 0, Dir: dir}, nil)
	if !errors.Is(err, vmidentity.ErrGoldenAux) {
		t.Fatalf("err %v, want ErrGoldenAux", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "ephemeral")); !os.IsNotExist(statErr) {
		t.Fatalf("blank golden left an aux tree: %v", statErr)
	}
	if got := mustRead(t, golden); len(got) != 0 {
		t.Fatalf("blank golden became %q", got)
	}
}

func TestColdBootRejectsSymlinkGolden(t *testing.T) {
	golden := writeGoldenAux(t, goldenAuxBytes)
	dir := t.TempDir()
	link := filepath.Join(dir, "aux-link")
	if err := os.Symlink(golden, link); err != nil {
		t.Fatal(err)
	}
	_, err := vmidentity.ColdBoot(vmidentity.Image{
		MachineIdentifierData: bakedMachineID,
		GoldenAux:             link,
	}, vmidentity.Pod{UID: "pod-link", Slot: 1, Dir: filepath.Join(dir, "pod")}, nil)
	if !errors.Is(err, vmidentity.ErrGoldenAux) {
		t.Fatalf("err %v, want ErrGoldenAux", err)
	}
	if got := mustRead(t, golden); !bytes.Equal(got, goldenAuxBytes) {
		t.Fatalf("golden aux = %q", got)
	}
}

func TestColdBootReleaseRemovesPodAuxOnly(t *testing.T) {
	golden := writeGoldenAux(t, goldenAuxBytes)
	boot := coldBoot(t, golden, t.TempDir(), "pod-release", 0)
	if err := boot.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(boot.AuxPath); !os.IsNotExist(err) {
		t.Fatalf("pod aux still present: %v", err)
	}
	if got := mustRead(t, golden); !bytes.Equal(got, goldenAuxBytes) {
		t.Fatalf("golden aux = %q after release", got)
	}
	if err := boot.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
}

func assertFreshIdentity(t *testing.T, boot vmidentity.Boot) {
	t.Helper()
	if !boot.AttachEntropy {
		t.Fatal("cold boot did not require a virtio entropy device")
	}
	if boot.MachineID == "" || boot.MachineID == bakedMachineID {
		t.Fatalf("machine id %q, want a fresh id", boot.MachineID)
	}
	if boot.DiscardedMachineID != bakedMachineID {
		t.Fatalf("discarded %q, want the baked image id", boot.DiscardedMachineID)
	}
}

func assertPrivateAux(t *testing.T, golden string, boot vmidentity.Boot, want []byte) {
	t.Helper()
	if boot.AuxPath == "" || boot.AuxPath == golden {
		t.Fatalf("aux path %q, want a pod-private clone of %s", boot.AuxPath, golden)
	}
	if got := mustRead(t, boot.AuxPath); !bytes.Equal(got, want) {
		t.Fatalf("aux %s = %q, want golden bytes", boot.AuxPath, got)
	}
}

func coldBoot(t *testing.T, golden, root, uid string, slot int) vmidentity.Boot {
	t.Helper()
	boot, err := vmidentity.ColdBoot(vmidentity.Image{
		MachineIdentifierData: bakedMachineID,
		GoldenAux:             golden,
	}, vmidentity.Pod{
		UID:  uid,
		Slot: slot,
		Dir:  filepath.Join(root, uid),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = boot.Release() })
	return boot
}

func writeGoldenAux(t *testing.T, payload []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "aux.img")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}
