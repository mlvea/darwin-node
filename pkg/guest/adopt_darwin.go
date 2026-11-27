//go:build darwin

package guest

/*
#include <stdint.h>
#include <mach/mach_time.h>
*/
import "C"

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"
)

// NewHostPlatform is the in-guest OS. On darwin it writes /dev/random,
// calls getentropy, sets the clock, and renames the machine. Tests must
// not call PerformAdopt on it: those steps change the Mac they run on.
func NewHostPlatform() AdoptPlatform {
	if softwarePlatformRequested() {
		return NewSoftwarePlatform()
	}
	return darwinAdopt{}
}

type darwinAdopt struct{}

func (darwinAdopt) ContinuousTime() (uint64, error) {
	return uint64(C.mach_continuous_time()), nil
}

func (darwinAdopt) WriteRandom(seed []byte) error {
	if len(seed) != AdoptEntropyLen {
		return fmt.Errorf("devrandom seed is %d bytes", len(seed))
	}
	f, err := os.OpenFile("/dev/random", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	n, err := f.Write(seed)
	if err != nil {
		return err
	}
	if n != len(seed) {
		return fmt.Errorf("devrandom wrote %d", n)
	}
	return nil
}

func (darwinAdopt) Entropy(n int) ([]byte, error) {
	if n <= 0 || n > 256 {
		return nil, fmt.Errorf("getentropy length %d", n)
	}
	b := make([]byte, n)
	_, _, errno := unix.Syscall(unix.SYS_GETENTROPY, uintptr(unsafe.Pointer(&b[0])), uintptr(n), 0)
	if errno != 0 {
		return nil, errno
	}
	return b, nil
}

func (darwinAdopt) SetWallClock(unixNano int64) error {
	sec := unixNano / int64(time.Second)
	usec := (unixNano % int64(time.Second)) / int64(time.Microsecond)
	tv := unix.Timeval{Sec: sec, Usec: int32(usec)}
	return unix.Settimeofday(&tv)
}

func (darwinAdopt) SetHostNames(hostname string) error {
	if err := ValidateHostname(hostname); err != nil {
		return err
	}
	local, err := localHostLabel(hostname)
	if err != nil {
		return err
	}
	sets := []struct{ key, val string }{
		{"HostName", hostname},
		{"LocalHostName", local},
		{"ComputerName", hostname},
	}
	for _, set := range sets {
		cmd := exec.Command("/usr/sbin/scutil", "--set", set.key, set.val)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("scutil %s: %w (%s)", set.key, err, bytes.TrimSpace(out))
		}
	}
	return nil
}

func (darwinAdopt) DeleteSSHHostKeys() error {
	return removeSSHHostKeys("/etc/ssh")
}

func (darwinAdopt) SignalMDNSResponder() error {
	cmd := exec.Command("/usr/bin/killall", "-HUP", "mDNSResponder")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mDNSResponder: %w (%s)", err, bytes.TrimSpace(out))
	}
	return nil
}

func (darwinAdopt) PlatformUUID() (string, error) {
	cmd := exec.Command("/usr/sbin/ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return parseIOPlatformUUID(string(out))
}

func (darwinAdopt) WallClock() (int64, error) {
	return time.Now().UnixNano(), nil
}

func (darwinAdopt) NumCPU() int { return runtime.NumCPU() }
