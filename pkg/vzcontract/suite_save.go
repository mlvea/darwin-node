package vzcontract

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// saveFile is the JSON descriptor SimVZ writes. The field names are part
// of the contract the replay and the mismatch test share.
type saveFile struct {
	Version       int    `json:"version"`
	Namespace     string `json:"namespace"`
	Name          string `json:"name"`
	UID           string `json:"uid"`
	HardwareModel string `json:"hardware_model"`
	MAC           string `json:"mac"`
	CPU           uint   `json:"cpu"`
	MemoryBytes   uint64 `json:"memory_bytes"`
}

func testSave(t *testing.T, factory Factory) {
	prepare(t)
	rt := factory(t, Config{})
	spec := specNamed("save")
	vm := mustCreate(t, rt, spec)
	mustStart(t, vm)
	err := vm.Save(bg(), filepath.Join(t.TempDir(), "early.json"))
	requireCode(t, err, CodeInvalidVirtualMachineState, NameInvalidState)
	if vm.State() != "Running" || !vm.QuotaHeld() || rt.Running() != 1 {
		t.Fatalf("save from running: %s held %v running %d", vm.State(), vm.QuotaHeld(), rt.Running())
	}
	wantEvents(t, suffix(vm.Events(), 1), []Event{evCode(CodeInvalidVirtualMachineState, NameInvalidState)})

	if err := vm.Pause(bg()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "good.json")
	if err := vm.Save(bg(), path); err != nil {
		t.Fatal(err)
	}
	if vm.State() != "Paused" || !vm.QuotaHeld() || rt.Running() != 1 {
		t.Fatalf("after save %s held %v running %d", vm.State(), vm.QuotaHeld(), rt.Running())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got saveFile
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := saveFile{
		Version:       1,
		Namespace:     spec.Namespace,
		Name:          spec.Name,
		UID:           spec.UID,
		HardwareModel: spec.HardwareModel,
		MAC:           spec.MAC,
		CPU:           spec.CPU,
		MemoryBytes:   spec.MemoryBytes,
	}
	if got != want {
		t.Fatalf("descriptor %+v, want %+v", got, want)
	}
	if err := vm.Stop(bg()); err != nil {
		t.Fatal(err)
	}

	bad := got
	bad.CPU = got.CPU + 1
	badPath := filepath.Join(t.TempDir(), "bad-cpu.json")
	writeSave(t, badPath, bad)
	err = vm.Restore(bg(), badPath)
	requireCode(t, err, CodeRestore, NameRestore)
	if vm.State() != "Stopped" || vm.QuotaHeld() || rt.Running() != 0 {
		t.Fatalf("bad restore %s held %v running %d", vm.State(), vm.QuotaHeld(), rt.Running())
	}

	bad.CPU = got.CPU
	bad.HardwareModel = "other-hw"
	hwPath := filepath.Join(t.TempDir(), "bad-hw.json")
	writeSave(t, hwPath, bad)
	err = vm.Restore(bg(), hwPath)
	requireCode(t, err, CodeRestore, NameRestore)
	if vm.State() != "Stopped" {
		t.Fatalf("state after hardware mismatch %s", vm.State())
	}

	if err := vm.Restore(bg(), path); err != nil {
		t.Fatal(err)
	}
	if vm.State() != "Paused" || !vm.QuotaHeld() || rt.Running() != 1 {
		t.Fatalf("good restore %s held %v running %d", vm.State(), vm.QuotaHeld(), rt.Running())
	}

	if err := vm.Stop(bg()); err != nil {
		t.Fatal(err)
	}
	other := mustCreate(t, rt, spec)
	mac := got
	mac.MAC = "02:00:00:00:00:ff"
	macPath := filepath.Join(t.TempDir(), "bad-mac.json")
	writeSave(t, macPath, mac)
	err = other.Restore(bg(), macPath)
	requireCode(t, err, CodeRestore, NameRestore)
	if other.State() != "Stopped" || other.QuotaHeld() {
		t.Fatalf("other %s held %v", other.State(), other.QuotaHeld())
	}
	if err := other.Restore(bg(), path); err != nil {
		t.Fatal(err)
	}
	if other.State() != "Paused" || !other.QuotaHeld() {
		t.Fatalf("new instance %s held %v", other.State(), other.QuotaHeld())
	}
}

func writeSave(t *testing.T, path string, doc saveFile) {
	t.Helper()
	body, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
}
