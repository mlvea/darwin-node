package vz

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/mlvea/darwin-node/pkg/types"
	"github.com/mlvea/darwin-node/pkg/vmidentity"
)

func TestPlanColdBootDiscardsBakedIDAndClonesAux(t *testing.T) {
	const baked = "baked-in-static-id"
	goldenBytes := []byte("GOLDEN-NVRAM-LOCALPOLICY-v1")
	root := t.TempDir()
	golden := filepath.Join(root, "image", "aux.img")
	if err := os.MkdirAll(filepath.Dir(golden), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(golden, goldenBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	seq := [2]vmidentity.Boot{}
	for i := range seq {
		spec, boot, err := planColdBoot(specFor(root, golden, baked, fmt.Sprintf("pod-seq-%d", i)), nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = boot.Release() })
		seq[i] = boot
		if boot.MachineID == "" || boot.MachineID == baked || spec.MachineIdentifierData != "" {
			t.Fatalf("pod %d id %q spec id %q", i, boot.MachineID, spec.MachineIdentifierData)
		}
		assertClonedAux(t, root, golden, goldenBytes, fmt.Sprintf("pod-seq-%d", i), spec.AuxPath)
	}
	if seq[0].MachineID == seq[1].MachineID {
		t.Fatalf("sequential pods share %q", seq[0].MachineID)
	}

	var wg sync.WaitGroup
	conc := [2]vmidentity.Boot{}
	specs := [2]types.VMSpec{}
	errs := [2]error{}
	for slot := 0; slot < 2; slot++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			specs[slot], conc[slot], errs[slot] = planColdBoot(specFor(root, golden, baked, fmt.Sprintf("slot-%d", slot)), nil)
		}(slot)
	}
	wg.Wait()
	for slot := range conc {
		if errs[slot] != nil {
			t.Fatalf("slot %d: %v", slot, errs[slot])
		}
		boot := conc[slot]
		t.Cleanup(func() { _ = boot.Release() })
		if boot.MachineID == "" || boot.MachineID == baked || specs[slot].MachineIdentifierData != "" {
			t.Fatalf("slot %d id %q spec id %q", slot, boot.MachineID, specs[slot].MachineIdentifierData)
		}
		assertClonedAux(t, root, golden, goldenBytes, fmt.Sprintf("slot-%d", slot), specs[slot].AuxPath)
	}
	if conc[0].MachineID == conc[1].MachineID || conc[0].AuxPath == conc[1].AuxPath {
		t.Fatalf("concurrent slots share id %q aux %s", conc[0].MachineID, conc[0].AuxPath)
	}
	ids := map[string]struct{}{
		seq[0].MachineID: {}, seq[1].MachineID: {},
		conc[0].MachineID: {}, conc[1].MachineID: {},
	}
	if len(ids) != 4 {
		t.Fatalf("machine ids collided: %d unique", len(ids))
	}

	mutated := []byte("SLOT0-NVRAM-CHANGED")
	if err := os.WriteFile(conc[0].AuxPath, mutated, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{golden, conc[1].AuxPath, seq[0].AuxPath} {
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, goldenBytes) {
			t.Fatalf("%s changed to %q", p, got)
		}
	}
	got, err := os.ReadFile(conc[0].AuxPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, mutated) {
		t.Fatalf("slot 0 aux = %q", got)
	}
}

func specFor(root, golden, baked, uid string) types.VMSpec {
	return types.VMSpec{
		ID:                    types.MachineID{Namespace: "ns", Name: uid, UID: uid},
		DiskPath:              filepath.Join(root, "pods", uid, "overlay", "disk.img"),
		AuxPath:               golden,
		MachineIdentifierData: baked,
		HardwareModelData:     "hw",
	}
}

func assertClonedAux(t *testing.T, root, golden string, goldenBytes []byte, uid, aux string) {
	t.Helper()
	want := filepath.Join(root, "pods", uid, "ephemeral", "slot-0", uid, "aux.img")
	if aux != want {
		t.Fatalf("aux %s, want %s", aux, want)
	}
	if aux == golden {
		t.Fatal("aux path is the golden image")
	}
	got, err := os.ReadFile(aux)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, goldenBytes) {
		t.Fatalf("aux bytes %q", got)
	}
}
