package guest

import "os/exec"

// ExitFromProcess maps a process wait error to a container exit code and a
// reason the host will keep. A nil error is 0, Completed. A non-zero exit
// is that status, Error. processExitCode turns a signal death into 128+N.
func ExitFromProcess(err error) (int32, string) {
	code := processExitCode(err)
	if code == 0 {
		return 0, "Completed"
	}
	return code, "Error"
}

// processExitCode is the portable part of a wait error. Signal deaths are
// handled in the unix file; other platforms only see ExitError.ExitCode.
func exitCodeFromError(err error) (int32, bool) {
	if err == nil {
		return 0, true
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		return 1, true
	}
	code := ee.ExitCode()
	if code < 0 {
		return 1, false
	}
	return int32(code), true
}
