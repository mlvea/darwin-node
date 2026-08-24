//go:build !darwin || !arm64

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func restoreIPSW(cmd *cobra.Command) error {
	return fmt.Errorf("IPSW restore requires darwin/arm64")
}
