package guest

import (
	"os/exec"
	"runtime"
	"strings"
)

// defaultMetalAvailable is true only when a Metal-capable accelerator is
// visible, not merely because the guest OS is darwin.
func defaultMetalAvailable() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	out, err := exec.Command("ioreg", "-c", "IOAccelerator", "-d", "1").Output()
	if err != nil {
		return false
	}
	s := string(out)
	return strings.Contains(s, "IOAccelerator") && (strings.Contains(s, "Metal") || strings.Contains(s, "AGX") || strings.Contains(s, "AppleM"))
}
