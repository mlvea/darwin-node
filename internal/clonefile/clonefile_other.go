//go:build !darwin

package clonefile

import "fmt"

func clonefile(src, dst string) error {
	return fmt.Errorf("clonefile not supported on this OS")
}
