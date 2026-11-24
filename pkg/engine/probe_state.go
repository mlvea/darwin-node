package engine

// probeStreak is the consecutive-sample state of one probe. The VM watch
// and the sidecar watch both use it, so successThreshold and failureThreshold
// mean the same thing for both.
//
// A success clears the failure streak. A failure clears the success streak.
// passed becomes true once successes reach successThreshold, and becomes
// false once failures reach failureThreshold. Below either threshold the
// previous passed value is left alone, which is what keeps a ready
// container ready until failureThreshold failures.
type probeStreak struct {
	oks    int32
	fails  int32
	passed bool
	msg    string
}

// observe records one sample. successTh and failTh below 1 are treated as 1.
// failed is true only when this sample made the failure streak reach failTh.
func (s *probeStreak) observe(ok bool, msg string, successTh, failTh int32) (passed, failed bool) {
	if s == nil {
		return false, false
	}
	if successTh < 1 {
		successTh = 1
	}
	if failTh < 1 {
		failTh = 1
	}
	if ok {
		s.fails = 0
		s.oks++
		if s.oks >= successTh {
			s.passed = true
			s.msg = ""
		}
		return s.passed, false
	}
	s.oks = 0
	s.fails++
	if msg == "" {
		msg = "probe failed"
	}
	s.msg = msg
	if s.fails >= failTh {
		s.passed = false
		return false, true
	}
	return s.passed, false
}
