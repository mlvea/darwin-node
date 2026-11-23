package guest

// ExitReport is one guest→host exit notice. The guest writes it on the
// agent connection it already has (vsock, or TCP when that fallback is on)
// and then halts. The host does not request it and does not reply.
//
// ExitCode is required. A missing or null code is not a report: the
// container stays at exit code 1. Zero is a real code. Reason is optional.
// The host keeps its own reason (VMStopped or VMFailed) when Reason is
// empty or not a Kubernetes-style reason. The last report that has a code
// wins. A later frame with no code does not clear it.
//
// The frame is:
//
//	{"v":1,"id":"exit","kind":"notify","method":"Exit",
//	 "payload":{"exitCode":137,"reason":"OOMKilled"}}
//
// Virtualization.framework does not report a process exit status, and the
// serial console is off unless --serial-console is set. That byte stream
// is the break-glass console, so it is not this channel. Shutdown is the
// host asking the guest to stop; an unexpected VM death never sends it.
// The guest has to say the code first, on this frame.
type ExitReport struct {
	ExitCode *int32 `json:"exitCode"`
	Reason   string `json:"reason,omitempty"`
}

// Reported is false when the guest omitted the code.
func (r ExitReport) Reported() bool { return r.ExitCode != nil }

// NormalizeExitReason keeps a short CamelCase reason and drops anything else.
// 128 matches a comfortable Kubernetes reason; longer text stays out of status.
func NormalizeExitReason(reason string) string {
	if reason == "" || len(reason) > 128 {
		return ""
	}
	for i := 0; i < len(reason); i++ {
		c := reason[i]
		letter := (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z')
		if i == 0 {
			if !letter {
				return ""
			}
			continue
		}
		if !letter && (c < '0' || c > '9') {
			return ""
		}
	}
	return reason
}

func exitNotifyEnvelope(rep ExitReport) (Envelope, error) {
	body, err := EncodePayload(rep)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{
		V:       ProtocolVersion,
		ID:      "exit",
		Kind:    KindNotify,
		Method:  MethodExit,
		Payload: body,
	}, nil
}

func offerExit(ch chan ExitReport, rep ExitReport) {
	select {
	case ch <- rep:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- rep:
	default:
	}
}

func (l *handlerLive) publishExit(rep ExitReport) {
	if l == nil || !rep.Reported() {
		return
	}
	code := *rep.ExitCode
	stored := ExitReport{ExitCode: &code, Reason: rep.Reason}
	l.mu.Lock()
	l.pendingExit = &stored
	sinks := append([]chan ExitReport(nil), l.exits...)
	l.mu.Unlock()
	for _, ch := range sinks {
		offerExit(ch, stored)
	}
}

func (l *handlerLive) addExitSink(ch chan ExitReport) func() {
	if l == nil {
		return func() {}
	}
	l.mu.Lock()
	l.exits = append(l.exits, ch)
	var pending *ExitReport
	if l.pendingExit != nil {
		cp := *l.pendingExit
		if cp.ExitCode != nil {
			code := *cp.ExitCode
			cp.ExitCode = &code
		}
		pending = &cp
	}
	l.mu.Unlock()
	if pending != nil {
		offerExit(ch, *pending)
	}
	return func() { l.removeExitSink(ch) }
}

func (l *handlerLive) removeExitSink(ch chan ExitReport) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]chan ExitReport, 0, len(l.exits))
	for _, c := range l.exits {
		if c != ch {
			out = append(out, c)
		}
	}
	l.exits = out
}
