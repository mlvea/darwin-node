//go:build darwin

package supervised

import (
	"fmt"
	"os"
	"os/exec"
)

func kickstart(slot int) error {
	label := fmt.Sprintf("gui/%d/io.darwin-node.slot-%d", os.Getuid(), slot)
	out, err := exec.Command("launchctl", "kickstart", label).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl kickstart %s: %w: %s", label, err, out)
	}
	return nil
}
