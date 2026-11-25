//go:build !linux

package simvz

import "os/exec"

func setDeathSignal(cmd *exec.Cmd) {}
