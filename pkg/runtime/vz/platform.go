//go:build darwin && arm64

package vz

import (
	"encoding/base64"
	"fmt"

	"github.com/Code-Hex/vz/v3"
	"github.com/darwin-node/darwin-node/pkg/types"
)

// attachPlatformAndDisk wires hardware model, machine identifier, auxiliary
// storage, and the boot disk. Derived from Agoda macOS-vz-kubelet
// pkg/vm/config (Apache-2.0).
func attachPlatformAndDisk(cfg *vz.VirtualMachineConfiguration, spec types.VMSpec) error {
	if spec.DiskPath == "" {
		return fmt.Errorf("vz runtime requires DiskPath (clonefile overlay of disk.img)")
	}
	if spec.HardwareModelData == "" {
		return fmt.Errorf("vz runtime requires HardwareModelData from the image config")
	}
	if spec.AuxPath == "" {
		return fmt.Errorf("vz runtime requires AuxPath (clonefile overlay of aux.img)")
	}

	hwRaw, err := base64.StdEncoding.DecodeString(spec.HardwareModelData)
	if err != nil {
		return fmt.Errorf("hardwareModelData: %w", err)
	}
	hw, err := vz.NewMacHardwareModelWithData(hwRaw)
	if err != nil {
		return fmt.Errorf("hardware model: %w", err)
	}

	var ident *vz.MacMachineIdentifier
	if spec.MachineIdentifierData != "" {
		idRaw, err := base64.StdEncoding.DecodeString(spec.MachineIdentifierData)
		if err != nil {
			return fmt.Errorf("machineIdData: %w", err)
		}
		ident, err = vz.NewMacMachineIdentifierWithData(idRaw)
		if err != nil {
			return fmt.Errorf("machine identifier: %w", err)
		}
	} else {
		// Always mint a unique identity for concurrent VMs (Apple requirement).
		ident, err = vz.NewMacMachineIdentifier()
		if err != nil {
			return fmt.Errorf("new machine identifier: %w", err)
		}
	}

	aux, err := vz.NewMacAuxiliaryStorage(spec.AuxPath)
	if err != nil {
		return fmt.Errorf("auxiliary storage: %w", err)
	}
	plat, err := vz.NewMacPlatformConfiguration(
		vz.WithMacAuxiliaryStorage(aux),
		vz.WithMacHardwareModel(hw),
		vz.WithMacMachineIdentifier(ident),
	)
	if err != nil {
		return fmt.Errorf("mac platform: %w", err)
	}
	cfg.SetPlatformVirtualMachineConfiguration(plat)

	disk, err := vz.NewDiskImageStorageDeviceAttachment(spec.DiskPath, false)
	if err != nil {
		return fmt.Errorf("disk attachment: %w", err)
	}
	block, err := vz.NewVirtioBlockDeviceConfiguration(disk)
	if err != nil {
		return fmt.Errorf("block device: %w", err)
	}
	cfg.SetStorageDevicesVirtualMachineConfiguration([]vz.StorageDeviceConfiguration{block})

	if err := attachInput(cfg); err != nil {
		return err
	}
	return nil
}

func attachInput(cfg *vz.VirtualMachineConfiguration) error {
	kb, err := vz.NewUSBKeyboardConfiguration()
	if err != nil {
		return err
	}
	cfg.SetKeyboardsVirtualMachineConfiguration([]vz.KeyboardConfiguration{kb})

	pointer, err := vz.NewUSBScreenCoordinatePointingDeviceConfiguration()
	if err != nil {
		return err
	}
	pointing := []vz.PointingDeviceConfiguration{pointer}
	if trackpad, err := vz.NewMacTrackpadConfiguration(); err == nil {
		pointing = append(pointing, trackpad)
	}
	cfg.SetPointingDevicesVirtualMachineConfiguration(pointing)
	return nil
}

func clampCPU(n uint) (uint, error) {
	max := vz.VirtualMachineConfigurationMaximumAllowedCPUCount()
	min := vz.VirtualMachineConfigurationMinimumAllowedCPUCount()
	if n > max {
		return 0, fmt.Errorf("cpu %d exceeds host maximum %d", n, max)
	}
	if n < min {
		return 0, fmt.Errorf("cpu %d below host minimum %d", n, min)
	}
	return n, nil
}

func clampMemory(n uint64) (uint64, error) {
	max := vz.VirtualMachineConfigurationMaximumAllowedMemorySize()
	min := vz.VirtualMachineConfigurationMinimumAllowedMemorySize()
	if n > max {
		return 0, fmt.Errorf("memory %d exceeds host maximum %d", n, max)
	}
	if n < min {
		return 0, fmt.Errorf("memory %d below host minimum %d", n, min)
	}
	return n, nil
}
