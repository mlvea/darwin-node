//go:build darwin

package supervisor

import (
	"errors"
	"fmt"
	"strings"

	"golang.org/x/sys/unix"
)

func HostBootID() (string, error) {
	s, err := unix.Sysctl("kern.bootsessionuuid")
	if err != nil {
		return "", err
	}
	s = strings.Trim(strings.TrimSpace(s), "\x00")
	if s == "" {
		return "", fmt.Errorf("host boot id is empty")
	}
	return s, nil
}

func ProcessStart(pid int) (int64, error) {
	if pid <= 0 {
		return 0, ErrProcGone
	}
	k, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) || errors.Is(err, unix.ENOENT) {
			return 0, ErrProcGone
		}
		return 0, err
	}
	if int(k.Proc.P_pid) != pid {
		return 0, ErrProcGone
	}
	start := k.Proc.P_starttime.Nano()
	if start == 0 {
		return 0, fmt.Errorf("proc start time is empty")
	}
	return start, nil
}
