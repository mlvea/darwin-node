// Package vmidentity decides per-pod machine identity and NVRAM for a
// cold boot. It does not import Virtualization.framework, so the policy
// runs in Linux tests. The darwin runtime turns the result into
// VZMacMachineIdentifier, VZMacAuxiliaryStorage, and a virtio entropy
// device.
//
// A cold boot never reuses the image config's machineIdData. Two
// concurrent VMs with one ECID are undefined in Virtualization.framework.
// NVRAM is a clone of the image's golden aux. A blank
// VZMacAuxiliaryStorage drops the iBoot LocalPolicy and the guest does
// not boot.
package vmidentity

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mlvea/darwin-node/internal/clonefile"
)

// ErrBakedMachineID is returned when a minter hands back the image's
// machine identifier. Cold boot refuses that value.
var ErrBakedMachineID = errors.New("minter returned the image machine identifier")

// ErrGoldenAux is returned when the image aux cannot be cloned. Cold
// boot does not create a blank auxiliary storage in its place.
var ErrGoldenAux = errors.New("golden aux")

// Image is the baked image a cold boot must not share identity with.
type Image struct {
	// MachineIdentifierData is the image config's machineIdData. Cold
	// boot records it and does not use it.
	MachineIdentifierData string
	// GoldenAux is the image aux.img that still has the iBoot LocalPolicy.
	GoldenAux string
}

// Pod is one cold-booted pod. Slot is 0 or 1, the host's two VM slots.
type Pod struct {
	UID  string
	Slot int
	// Dir is the pod directory. The aux clone is created under it.
	Dir string
}

// Minter returns one fresh machine-identifier string. A nil minter reads
// crypto/rand. The darwin runtime passes a minter that calls
// vz.NewMacMachineIdentifier; those bytes are what the VM boots with.
type Minter func() (string, error)

// Boot is the per-pod identity and NVRAM clone for one cold boot.
type Boot struct {
	// MachineID is a newly minted identifier, never the image's.
	MachineID string
	// DiscardedMachineID is the image value that was not used.
	DiscardedMachineID string
	// AuxPath is the pod-private clone of the golden aux.
	AuxPath string
	// AttachEntropy is true. The VM config must include a virtio entropy
	// device. Virtualization.framework has no vmgenid on restore, so this
	// device is how a later snapshot boot can reseed the guest.
	AttachEntropy bool
	Slot          int
	UID           string

	golden string
}

// ColdBoot mints a machine identifier and clonefiles the golden aux into
// a pod-private path. It does not create a file when the golden aux is
// missing or empty, and it does not accept a minter that returns the
// image identifier.
func ColdBoot(img Image, pod Pod, mint Minter) (Boot, error) {
	aux, err := podAuxPath(pod)
	if err != nil {
		return Boot{}, err
	}
	if img.GoldenAux == "" {
		return Boot{}, fmt.Errorf("%w: path is empty", ErrGoldenAux)
	}
	same, err := sameFile(img.GoldenAux, aux)
	if err != nil {
		return Boot{}, err
	}
	if same {
		return Boot{}, fmt.Errorf("%w: refusing to clone %s onto itself", ErrGoldenAux, img.GoldenAux)
	}
	if err := readableAux(img.GoldenAux); err != nil {
		return Boot{}, err
	}
	if mint == nil {
		mint = defaultMint
	}
	id, err := freshID(mint, img.MachineIdentifierData)
	if err != nil {
		return Boot{}, err
	}
	if err := cloneAux(img.GoldenAux, aux); err != nil {
		return Boot{}, err
	}
	return Boot{
		MachineID:          id,
		DiscardedMachineID: img.MachineIdentifierData,
		AuxPath:            aux,
		AttachEntropy:      true,
		Slot:               pod.Slot,
		UID:                pod.UID,
		golden:             img.GoldenAux,
	}, nil
}

// Release removes the pod-private aux clone. The golden image is left
// in place. A second call is a no-op.
func (b Boot) Release() error {
	if b.AuxPath == "" {
		return nil
	}
	if b.golden != "" {
		same, err := sameFile(b.AuxPath, b.golden)
		if err != nil {
			return err
		}
		if same {
			return fmt.Errorf("%w: refusing to remove %s", ErrGoldenAux, b.golden)
		}
	}
	err := os.Remove(b.AuxPath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func podAuxPath(pod Pod) (string, error) {
	if pod.UID == "" {
		return "", fmt.Errorf("pod uid is empty")
	}
	if pod.Dir == "" {
		return "", fmt.Errorf("pod dir is empty")
	}
	if pod.Slot != 0 && pod.Slot != 1 {
		return "", fmt.Errorf("vm slot %d is outside 0 and 1", pod.Slot)
	}
	if pod.UID != filepath.Base(pod.UID) || pod.UID == "." || pod.UID == ".." {
		return "", fmt.Errorf("pod uid %q is not a single path element", pod.UID)
	}
	return filepath.Join(pod.Dir, "ephemeral", fmt.Sprintf("slot-%d", pod.Slot), pod.UID, "aux.img"), nil
}

func freshID(mint Minter, baked string) (string, error) {
	var last string
	var lastErr error
	for i := 0; i < 4; i++ {
		id, err := mint()
		if err != nil {
			lastErr = err
			continue
		}
		last = id
		if id != "" && id != baked {
			return id, nil
		}
	}
	if last != "" && last == baked {
		return "", ErrBakedMachineID
	}
	if lastErr != nil {
		return "", fmt.Errorf("mint machine identifier: %w", lastErr)
	}
	return "", fmt.Errorf("minter returned an empty machine identifier")
}

func defaultMint() (string, error) {
	buf := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, buf); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

func readableAux(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrGoldenAux, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrGoldenAux, path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("%w: refusing blank %s", ErrGoldenAux, path)
	}
	return nil
}

func cloneAux(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrGoldenAux, err)
	}
	if err := clonefile.File(src, dst); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("%w: %w", ErrGoldenAux, err)
	}
	dstInfo, err := os.Lstat(dst)
	if err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("%w: %w", ErrGoldenAux, err)
	}
	if dstInfo.Mode()&os.ModeSymlink != 0 || !dstInfo.Mode().IsRegular() || dstInfo.Size() == 0 || dstInfo.Size() != info.Size() {
		_ = os.Remove(dst)
		return fmt.Errorf("%w: cloned %s is not a copy of the golden aux", ErrGoldenAux, dst)
	}
	return nil
}

func sameFile(a, b string) (bool, error) {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true, nil
	}
	ai, err := os.Lstat(a)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	bi, err := os.Lstat(b)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return os.SameFile(ai, bi), nil
}
