package vz

import (
	"fmt"
	"path/filepath"

	"github.com/mlvea/darwin-node/pkg/types"
	"github.com/mlvea/darwin-node/pkg/vmidentity"
)

// planColdBoot clones the image aux to a pod-private path and mints a
// machine identifier. The image MachineIdentifierData is passed through
// so ColdBoot can reject it. VMSpec carries no capacity-slot index. The
// pod UID is unique across the two concurrent VMs, so this always asks
// for slot 0; the helper still keeps an explicit slot 1 distinct when a
// caller knows the slot.
func planColdBoot(spec types.VMSpec, mint vmidentity.Minter) (types.VMSpec, vmidentity.Boot, error) {
	if spec.DiskPath == "" {
		return spec, vmidentity.Boot{}, fmt.Errorf("vz runtime requires DiskPath (clonefile overlay of disk.img)")
	}
	if spec.AuxPath == "" {
		return spec, vmidentity.Boot{}, fmt.Errorf("vz runtime requires AuxPath (clonefile overlay of aux.img)")
	}
	if spec.ID.UID == "" {
		return spec, vmidentity.Boot{}, fmt.Errorf("vz runtime requires a pod uid to clone aux")
	}
	boot, err := vmidentity.ColdBoot(vmidentity.Image{
		MachineIdentifierData: spec.MachineIdentifierData,
		GoldenAux:             spec.AuxPath,
	}, vmidentity.Pod{
		UID:  spec.ID.UID,
		Slot: 0,
		Dir:  podDirectory(spec.DiskPath),
	}, mint)
	if err != nil {
		return spec, vmidentity.Boot{}, err
	}
	spec.AuxPath = boot.AuxPath
	spec.MachineIdentifierData = ""
	return spec, boot, nil
}

// podDirectory is the pod cache directory when the disk lives in the
// engine's overlay folder, and the disk's directory otherwise. The aux
// clone is created under it and removed on Stop.
func podDirectory(diskPath string) string {
	overlay := filepath.Dir(diskPath)
	if filepath.Base(overlay) == "overlay" {
		return filepath.Dir(overlay)
	}
	return overlay
}
