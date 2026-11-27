//go:build !linux && !darwin

package supervisor

import "fmt"

func HostBootID() (string, error) {
	return "", fmt.Errorf("host boot id is not available")
}

func ProcessStart(int) (int64, error) {
	return 0, fmt.Errorf("process start time is not available")
}
