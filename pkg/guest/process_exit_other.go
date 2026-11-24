//go:build !unix

package guest

// processExitCode reports the container exit code for a wait error.
// Platforms without syscall.WaitStatus cannot see a signal number.
func processExitCode(err error) int32 {
	code, _ := exitCodeFromError(err)
	return code
}
