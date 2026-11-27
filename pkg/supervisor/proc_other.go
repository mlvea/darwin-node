//go:build !linux && !darwin

package supervisor

import "fmt"

type procHandle struct{}

func openProcWatch(int) (*procHandle, error) {
	return nil, fmt.Errorf("process watch is not implemented")
}

func (h *procHandle) close() error { return nil }

func (h *procHandle) wait(int) (bool, error) {
	return false, fmt.Errorf("process watch is not implemented")
}
