package engine

import "time"

// SetAgentRedialPaceForTest replaces the wall-clock delays in the guest
// redial loop. Those waits are not probe timers: the session is blocked in
// a socket read, so the virtual clock cannot wake them. The lifecycle
// fuzzer uses a few milliseconds so Delete is not stuck behind a 5s
// backoff while the 5s deadlock watchdog is running. Production keeps the
// values in agent_session.go.
//
// The returned function restores the previous pace. Do not call this from
// tests that run in parallel with each other; the pace is process-global.
func SetAgentRedialPaceForTest(backoff, max, budget time.Duration) (restore func()) {
	prevB, prevM, prevG := agentRedialBackoff, agentRedialMax, agentRedialBudget
	if backoff > 0 {
		agentRedialBackoff = backoff
	}
	if max > 0 {
		agentRedialMax = max
	}
	if budget > 0 {
		agentRedialBudget = budget
	}
	return func() {
		agentRedialBackoff = prevB
		agentRedialMax = prevM
		agentRedialBudget = prevG
	}
}
