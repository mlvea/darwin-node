//go:build darwin && arm64

package vz

import (
	"encoding/base64"
	"fmt"
	"runtime"

	"github.com/Code-Hex/vz/v3"
	"github.com/mlvea/darwin-node/pkg/types"
	"github.com/mlvea/darwin-node/pkg/vmidentity"
)

// preparePlatform mints one vz.NewMacMachineIdentifier per cold boot
// and passes it to planColdBoot. That helper clonefiles NVRAM and fails
// closed when the minted value equals the image identifier. The VM is
// configured with the minted object. Whether two guests report different
// IOPlatformUUID values is hardware-pending.
func preparePlatform(spec types.VMSpec) (types.VMSpec, vmidentity.Boot, *vz.MacMachineIdentifier, error) {
	ident, err := vz.NewMacMachineIdentifier()
	if err != nil {
		return spec, vmidentity.Boot{}, nil, err
	}
	raw := append([]byte(nil), ident.DataRepresentation()...)
	runtime.KeepAlive(ident)
	if len(raw) == 0 {
		return spec, vmidentity.Boot{}, nil, fmt.Errorf("Virtualization.framework returned an empty machine identifier")
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	next, boot, err := planColdBoot(spec, func() (string, error) {
		return encoded, nil
	})
	if err != nil {
		return spec, boot, nil, err
	}
	if boot.MachineID != encoded {
		_ = boot.Release()
		return spec, vmidentity.Boot{}, nil, fmt.Errorf("cold boot did not keep the fresh machine identifier")
	}
	return next, boot, ident, nil
}

// attachPlatformAndDisk wires hardware model, machine identifier, auxiliary
// storage, the entropy device, and the boot disk. Derived from Agoda
// macOS-vz-kubelet pkg/vm/config (Apache-2.0). The machine identifier is
// the one preparePlatform minted. WithCreatingMacAuxiliaryStorage is not
// used: a blank aux drops the iBoot LocalPolicy and the guest will not boot.
func attachPlatformAndDisk(cfg *vz.VirtualMachineConfiguration, spec types.VMSpec, boot vmidentity.Boot, ident *vz.MacMachineIdentifier) error {
	if spec.DiskPath == "" {
		return fmt.Errorf("vz runtime requires DiskPath (clonefile overlay of disk.img)")
	}
	if spec.HardwareModelData == "" {
		return fmt.Errorf("vz runtime requires HardwareModelData from the image config")
	}
	if boot.AuxPath == "" {
		return fmt.Errorf("vz runtime requires a cloned aux image")
	}
	if !boot.AttachEntropy {
		return fmt.Errorf("cold boot requires a virtio entropy device")
	}
	if ident == nil || boot.MachineID == "" || (boot.DiscardedMachineID != "" && boot.MachineID == boot.DiscardedMachineID) {
		return fmt.Errorf("refusing to boot with the image machine identifier")
	}

	hwRaw, err := base64.StdEncoding.DecodeString(spec.HardwareModelData)
	if err != nil {
		return fmt.Errorf("hardwareModelData: %w", err)
	}
	hw, err := vz.NewMacHardwareModelWithData(hwRaw)
	if err != nil {
		return fmt.Errorf("hardware model: %w", err)
	}

	// ident is the vz.NewMacMachineIdentifier preparePlatform just minted.
	// spec.MachineIdentifierData is cleared before this call and is not
	// decoded. IOPlatformUUID differing across boots is hardware-pending.
	aux, err := vz.NewMacAuxiliaryStorage(boot.AuxPath)
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

	entropy, err := vz.NewVirtioEntropyDeviceConfiguration()
	if err != nil {
		return fmt.Errorf("entropy device: %w", err)
	}
	cfg.SetEntropyDevicesVirtualMachineConfiguration([]*vz.VirtioEntropyDeviceConfiguration{entropy})

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
