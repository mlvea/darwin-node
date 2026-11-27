package supervisor

// RaiseNOFILE raises RLIMIT_NOFILE to 65536, or to the hard limit when
// that is lower. It does not lower a soft limit that is already higher.
//
// The red stub does not change the limit. Startup of the node-agent and
// the supervisor both call this.
func RaiseNOFILE() error {
	return nil
}
