package supervisor

// SetCapabilities replaces the advertised capability list. Call it
// before Serve. An empty list advertises nothing, which is how a
// minor-behind supervisor declines pause. Nil uses the defaults.
func (s *Server) SetCapabilities(list []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.capSet = true
	s.capList = append([]string(nil), list...)
}

func (s *Server) stampVersion(msg *message) {
	if s == nil || msg == nil {
		return
	}
	major := s.AdvertiseWireMajor
	if major == 0 {
		major = WireMajor
	}
	minor := s.AdvertiseWireMinor
	if minor == 0 {
		minor = WireMinor
	}
	sha := s.BuildSHA
	if sha == "" {
		sha = s.version
	}
	s.mu.Lock()
	caps := defaultCapabilities()
	if s.capSet {
		caps = append([]string(nil), s.capList...)
	}
	started := s.workloadStarted
	exited := s.workloadExited
	var code *int32
	if s.workloadCode != nil {
		c := *s.workloadCode
		code = &c
	}
	podUID := s.podUID
	s.mu.Unlock()
	msg.WireMajor = &major
	msg.WireMinor = minor
	msg.BuildSHA = sha
	msg.Capabilities = caps
	msg.HostBootID = s.hostBoot
	msg.WorkloadStarted = started
	msg.WorkloadExited = exited
	if podUID != "" {
		msg.PodUID = podUID
	}
	if code != nil {
		msg.ExitCode = code
	}
}

func (s *Server) handleNoteWorkload(req message) (message, error) {
	s.mu.Lock()
	if req.WorkloadStarted {
		s.workloadStarted = true
	}
	if req.WorkloadExited {
		s.workloadExited = true
		if req.ExitCode != nil {
			c := *req.ExitCode
			s.workloadCode = &c
		}
	}
	s.mu.Unlock()
	msg := s.inspect()
	msg.Op = opResponse
	msg.OK = true
	return msg, nil
}
