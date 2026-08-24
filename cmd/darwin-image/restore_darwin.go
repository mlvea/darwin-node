//go:build darwin && arm64

package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func restoreIPSW(cmd *cobra.Command) error {
	ipsw, _ := cmd.Flags().GetString("ipsw")
	out, _ := cmd.Flags().GetString("out")
	if ipsw == "" || out == "" {
		return fmt.Errorf("restore requires --ipsw and --out; uses VZMacOSRestoreImage + VZMacOSInstaller on this host")
	}
	return fmt.Errorf("restore %s -> %s: run on a signed darwin-node host; VZMacOSInstaller is interactive-length (30–60m) and is invoked from pkg/runtime/vz in a follow-up hardware job", ipsw, out)
}
