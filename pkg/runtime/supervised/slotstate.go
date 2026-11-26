package supervised

import "github.com/mlvea/darwin-node/pkg/runtime"

// SetAgentToken is the guest handshake token DialAgent sends. Recover
// does not have it: the lease stores the boot fence, not the agent token.
// The engine reads the token back from the pod directory and sets it
// before redialing.
func (m *machine) SetAgentToken(token string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.spec.AgentToken = token
	m.mu.Unlock()
}

// Abandon closes this agent's control connection and the guest fd from
// its Adopt without stopping the VM. A node-agent crash does the same.
// The supervisor keeps the machine so a later Recover can adopt it.
func Abandon(m runtime.Machine) {
	mm, ok := m.(*machine)
	if !ok || mm == nil {
		return
	}
	mm.dropConn()
}

// HeldSlots is how many supervisor slots this adapter has reserved.
func (r *Runtime) HeldSlots() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensure()
	n := 0
	for _, used := range r.used {
		if used {
			n++
		}
	}
	return n
}

// FreeSlots is how many supervisor slots this adapter can still reserve.
// The cap is Options.Slots, or the Apple maximum of 2 when that is unset.
func (r *Runtime) FreeSlots() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensure()
	n := 0
	for _, used := range r.used {
		if !used {
			n++
		}
	}
	return n
}
