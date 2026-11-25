//go:build !darwin

package supervised

import "fmt"

func kickstart(slot int) error {
	return fmt.Errorf("launchd slot %d is only available on darwin", slot)
}
