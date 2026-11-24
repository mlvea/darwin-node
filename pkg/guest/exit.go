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

// exitNote is one Exit report plus the sequence a waiter matches.
// seq is 0 when nobody is waiting for this write.
type exitNote struct {
	rep ExitReport
	seq uint64
}

// exitSink is one live agent connection. close unblocks a write that a
// dead host will never read, so the halt after a workload exit can proceed.
type exitSink struct {
	ch    chan exitNote
	close func()
}

func offerNote(ch chan exitNote, note exitNote) {
	select {
	case ch <- note:
		return
	default:
	}
	select {
	case <-ch:
	default:
	}
	select {
	case ch <- note:
	default:
	}
}

func (l *handlerLive) publishExit(rep ExitReport) {
	l.publish(rep, false)
}

// publishExitWait stores rep and returns a channel that is closed after a
// connection writes it or the write fails. No connection yet leaves the
// channel open until one accepts and writes the replayed report.
func (l *handlerLive) publishExitWait(rep ExitReport) <-chan struct{} {
	return l.publish(rep, true)
}

func (l *handlerLive) publish(rep ExitReport, wait bool) <-chan struct{} {
	if l == nil || !rep.Reported() {
		if !wait {
			return nil
		}
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	code := *rep.ExitCode
	stored := ExitReport{ExitCode: &code, Reason: rep.Reason}
	var done chan struct{}
	l.mu.Lock()
	l.pendingExit = &stored
	l.exitSeq++
	noteSeq := uint64(0)
	if wait {
		if l.exitWait != nil {
			close(l.exitWait)
			l.exitWait = nil
		}
		done = make(chan struct{})
		l.exitWait = done
		l.waitSeq = l.exitSeq
		l.pendingSeq = l.exitSeq
		noteSeq = l.exitSeq
	} else {
		l.pendingSeq = 0
	}
	sinks := append([]exitSink(nil), l.exits...)
	l.mu.Unlock()
	note := exitNote{rep: stored, seq: noteSeq}
	for _, s := range sinks {
		offerNote(s.ch, note)
	}
	return done
}

// finishExitWait closes the waiter for seq. A different report's write
// does not release it.
func (l *handlerLive) finishExitWait(seq uint64) {
	if l == nil || seq == 0 {
		return
	}
	l.mu.Lock()
	if l.exitWait == nil || seq != l.waitSeq {
		l.mu.Unlock()
		return
	}
	ch := l.exitWait
	l.exitWait = nil
	l.mu.Unlock()
	close(ch)
}

// closeExitConns closes every agent connection so a blocked Exit write
// returns. The count is how many connections were closed.
func (l *handlerLive) closeExitConns() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	closers := make([]func(), 0, len(l.exits))
	for _, s := range l.exits {
		if s.close != nil {
			closers = append(closers, s.close)
		}
	}
	l.mu.Unlock()
	for _, c := range closers {
		c()
	}
	return len(closers)
}

func (l *handlerLive) addExitSink(ch chan exitNote, closeFn func()) func() {
	if l == nil {
		return func() {}
	}
	l.mu.Lock()
	l.exits = append(l.exits, exitSink{ch: ch, close: closeFn})
	var note *exitNote
	if l.pendingExit != nil && l.pendingExit.ExitCode != nil {
		cp := *l.pendingExit
		code := *cp.ExitCode
		cp.ExitCode = &code
		n := exitNote{rep: cp, seq: l.pendingSeq}
		note = &n
	}
	l.mu.Unlock()
	if note != nil {
		offerNote(ch, *note)
	}
	return func() { l.removeExitSink(ch) }
}

func (l *handlerLive) removeExitSink(ch chan exitNote) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]exitSink, 0, len(l.exits))
	for _, s := range l.exits {
		if s.ch != ch {
			out = append(out, s)
		}
	}
	l.exits = out
}
