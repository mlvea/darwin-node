package supervisor

import "golang.org/x/sys/unix"

// nofileTarget is the soft RLIMIT_NOFILE the node-agent and each
// supervisor raise to at startup. A lower hard limit wins. A soft limit
// that is already higher is left alone.
const nofileTarget = 65536

// RaiseNOFILE raises RLIMIT_NOFILE to nofileTarget, or to the hard limit
// when that is lower. It does not lower a soft limit that is already
// higher. It is safe to call more than once.
func RaiseNOFILE() error {
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		return err
	}
	target := uint64(nofileTarget)
	if lim.Max < target {
		target = lim.Max
	}
	if lim.Cur >= target {
		return nil
	}
	lim.Cur = target
	return unix.Setrlimit(unix.RLIMIT_NOFILE, &lim)
}
