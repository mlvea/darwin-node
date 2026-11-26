package simvz

import (
	"errors"
	"fmt"
)

// ErrNoRunningGuest means CrashGuest found no Running or Paused VM to kill.
var ErrNoRunningGuest = errors.New("simvz: no running guest")

// FailNextStarts adds n to the runtime-wide start-failure budget. Each
// later Start, including one on a machine created after this call, opens
// the NIC and the vsock listener and then fails until the budget is gone.
// n <= 0 does nothing. The same counter backs Faults.FailStart.
func (r *Runtime) FailNextStarts(n int) {
	if r == nil || n <= 0 {
		return
	}
	r.mu.Lock()
	r.failLeft += n
	r.mu.Unlock()
}

// CrashGuest kills the guest of one live VM and leaves the machine in its
// current state so the slot stays held. namespace and name select the VM.
// Both empty selects the first Running or Paused machine. A missing target
// returns ErrNoRunningGuest and does not change any VM.
func (r *Runtime) CrashGuest(namespace, name string) error {
	if r == nil {
		return fmt.Errorf("simvz: nil runtime")
	}
	m := r.findLive(namespace, name)
	if m == nil {
		return ErrNoRunningGuest
	}
	return m.Crash()
}

// LiveCloneDirs lists per-VM clone directories that cleanup has not removed.
// The lifecycle fuzzer compares this with the directory on disk.
func (r *Runtime) LiveCloneDirs() []string {
	if r == nil {
		return nil
	}
	var out []string
	for _, m := range r.machinesSnapshot() {
		if m == nil {
			continue
		}
		m.mu.Lock()
		dir := m.dir
		m.mu.Unlock()
		if dir != "" {
			out = append(out, dir)
		}
	}
	return out
}

func (r *Runtime) track(m *machine) {
	if r == nil || m == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, cur := range r.machines {
		if cur == m {
			return
		}
	}
	r.machines = append(r.machines, m)
}

func (r *Runtime) untrack(m *machine) {
	if r == nil || m == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, cur := range r.machines {
		if cur != m {
			r.machines[n] = cur
			n++
		}
	}
	for i := n; i < len(r.machines); i++ {
		r.machines[i] = nil
	}
	r.machines = r.machines[:n]
}

func (r *Runtime) machinesSnapshot() []*machine {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*machine, len(r.machines))
	copy(out, r.machines)
	return out
}

// findLive returns a Running or Paused machine. A non-empty name restricts
// the match to that namespace and name. The snapshot is taken without the
// machine lock so cleanup can untrack concurrently.
func (r *Runtime) findLive(namespace, name string) *machine {
	for _, m := range r.machinesSnapshot() {
		if m == nil {
			continue
		}
		m.mu.Lock()
		st := m.state
		id := m.id
		m.mu.Unlock()
		if st != vzRunning && st != vzPaused {
			continue
		}
		if name != "" && (id.Namespace != namespace || id.Name != name) {
			continue
		}
		return m
	}
	return nil
}
