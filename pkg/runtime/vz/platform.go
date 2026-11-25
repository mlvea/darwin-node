//go:build darwin && arm64

package vz

import (
	"encoding/base64"
	"fmt"

	"github.com/Code-Hex/vz/v3"
	"github.com/mlvea/darwin-node/pkg/types"
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
	// macOS 13 guests use the trackpad. Skipping a failed constructor was
	// the macOS 12 reservation; the host floor is 13, so this is fatal.
	trackpad, err := vz.NewMacTrackpadConfiguration()
	if err != nil {
		return err
	}
	pointing = append(pointing, trackpad)
	cfg.SetPointingDevicesVirtualMachineConfiguration(pointing)
	return nil
}

// MinimumHostDarwin is the oldest host release this runtime will boot.
// MacOSGuestAutomountTag and guest AF_VSOCK both start at macOS 13.
const MinimumHostDarwin = 13

// Available reports whether this host can boot a darwin-node VM.
// It refuses a host older than MinimumHostDarwin, and a host whose
// Virtualization.framework cannot size a VM. It does not start a VM.
func Available() error {
	if _, err := vz.MacOSGuestAutomountTag(); err != nil {
		return fmt.Errorf("darwin-node requires host macOS %d or newer (virtio-fs automount and guest vsock): %w", MinimumHostDarwin, err)
	}
	min := vz.VirtualMachineConfigurationMinimumAllowedCPUCount()
	max := vz.VirtualMachineConfigurationMaximumAllowedCPUCount()
	if min == 0 || max == 0 || min > max {
		return fmt.Errorf("Virtualization.framework is not available")
	}
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
