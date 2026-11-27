//go:build linux

package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

func HostBootID() (string, error) {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return "", fmt.Errorf("host boot id is empty")
	}
	return s, nil
}

func ProcessStart(pid int) (int64, error) {
	if pid <= 0 {
		return 0, ErrProcGone
	}
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, ErrProcGone
		}
		return 0, err
	}
	return startTicks(b)
}

// startTicks reads field 22 of /proc/<pid>/stat. The command name is
// everything up to the last ')', because a comm can contain spaces and
// parentheses.
func startTicks(stat []byte) (int64, error) {
	i := bytes.LastIndexByte(stat, ')')
	if i < 0 || i+2 >= len(stat) {
		return 0, fmt.Errorf("proc stat")
	}
	fields := strings.Fields(string(stat[i+2:]))
	const startField = 19 // field 22, 0-based among the fields after comm
	if len(fields) <= startField {
		return 0, fmt.Errorf("proc stat")
	}
	n, err := strconv.ParseInt(fields[startField], 10, 64)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, fmt.Errorf("proc start time is empty")
	}
	return n, nil
}
