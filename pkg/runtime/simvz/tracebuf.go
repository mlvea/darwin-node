package simvz

import (
	"errors"
	"syscall"
)

// traceEvent is one normalized transition the contract suite reads back.
// Completion handlers always carry an error value: nil means success.
type traceEvent struct {
	Kind  string
	State string
	Err   *traceErr
}

// traceErr is the envelope of a completion. Errno is set for a fast
// connect refusal. Domain and Code are a VZErrorDomain NSError.
type traceErr struct {
	Domain string
	Code   int
	Name   string
	Errno  string
}

func (m *machine) record(ev traceEvent) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.events = append(m.events, ev)
	m.mu.Unlock()
}

func (m *machine) recordCompletion(err error) {
	ev := traceEvent{Kind: "completion_handler"}
	if err != nil {
		ev.Err = traceErrFrom(err)
	}
	m.record(ev)
}

func traceErrFrom(err error) *traceErr {
	if err == nil {
		return nil
	}
	var coded vzCoded
	if errors.As(err, &coded) && coded != nil {
		return &traceErr{Domain: coded.Domain(), Code: coded.Code(), Name: coded.FrameworkName()}
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return &traceErr{Errno: "ECONNREFUSED"}
	}
	if errors.Is(err, syscall.EPIPE) {
		return &traceErr{Errno: "EPIPE"}
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return &traceErr{Errno: "ECONNRESET"}
	}
	return &traceErr{Name: err.Error()}
}

func (m *machine) eventsSnapshot() []traceEvent {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]traceEvent, len(m.events))
	copy(out, m.events)
	return out
}

// commitLocked records a state_change and reports whether the caller
// must release a held VM slot. The machine lock is held by the caller.
func (m *machine) commitLocked(to vzState) (release bool) {
	m.state = to
	m.events = append(m.events, traceEvent{Kind: "state_change", State: to.String()})
	if m.held && !to.holdsQuota() {
		m.held = false
		return true
	}
	return false
}

func (m *machine) releaseSlot(release bool) {
	if release && m.rt != nil {
		m.rt.release()
	}
}

func (m *machine) become(to vzState) {
	m.mu.Lock()
	release := m.commitLocked(to)
	m.mu.Unlock()
	m.releaseSlot(release)
}
