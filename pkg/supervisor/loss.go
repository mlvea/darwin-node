package supervisor

// watchLoss reports supervisor death from EVFILT_PROC / pidfd and from
// ctrl EOF. lost must run once for the whole set of signals.
//
// The red stub never reports a loss.
func watchLoss(pid, ctrlFD, guestFD int, lost func()) (stop func(), err error) {
	_, _, _, _ = pid, ctrlFD, guestFD, lost
	return func() {}, nil
}
