// Package capacity enforces the hard 2-VM Apple limit and fail-closed admission.
package capacity

import (
	"errors"
	"fmt"
	"sync"

	"github.com/darwin-node/darwin-node/pkg/types"
)

var (
	// ErrInvalidMax is returned when max VMs is outside 1..AppleMaxConcurrentVMs.
	ErrInvalidMax = errors.New("max VMs must be 1 or 2")
	// ErrVMCapacityExhausted is returned when both slots are taken.
	ErrVMCapacityExhausted = errors.New("VM capacity exhausted: this host allows at most 2 concurrent macOS VMs (Apple EULA / Virtualization.framework)")
	// ErrNotHeld is returned when Release is called for an unknown UID.
	ErrNotHeld = errors.New("vm slot not held")
)

// Slots is the in-process table of VM slots. It is the only thing allowed
// to decide whether a macOS VM may be created.
type Slots struct {
	max int

	mu   sync.Mutex
	held map[string]struct{} // pod UID
}

// New validates max and returns a slot table.
func New(max int) (*Slots, error) {
	if max < 1 || max > types.AppleMaxConcurrentVMs {
		return nil, fmt.Errorf("%w (got %d)", ErrInvalidMax, max)
	}
	return &Slots{
		max:  max,
		held: make(map[string]struct{}, max),
	}, nil
}

// Max returns the configured cap (1 or 2).
func (s *Slots) Max() int { return s.max }

// Used returns how many slots are currently held.
func (s *Slots) Used() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.held)
}

// Free returns remaining slots.
func (s *Slots) Free() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max - len(s.held)
}

// Full reports whether no slot remains.
func (s *Slots) Full() bool { return s.Free() == 0 }

// Holds reports whether uid already owns a slot (idempotent Create).
func (s *Slots) Holds(uid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.held[uid]
	return ok
}

// TryAcquire takes a slot for uid. Idempotent if uid already holds one.
// Never blocks. A third distinct uid fails with ErrVMCapacityExhausted.
func (s *Slots) TryAcquire(uid string) error {
	if uid == "" {
		return fmt.Errorf("empty pod uid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.held[uid]; ok {
		return nil
	}
	if len(s.held) >= s.max {
		return ErrVMCapacityExhausted
	}
	s.held[uid] = struct{}{}
	return nil
}

// Release frees the slot. Missing uid is not an error (delete is idempotent).
func (s *Slots) Release(uid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.held, uid)
}

// Rebuild replaces the table (crash recovery). UIDs beyond max are refused.
func (s *Slots) Rebuild(uids []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := make(map[string]struct{}, s.max)
	for _, u := range uids {
		if u == "" {
			continue
		}
		if len(next) >= s.max {
			return ErrVMCapacityExhausted
		}
		next[u] = struct{}{}
	}
	s.held = next
	return nil
}

// UIDs returns a snapshot of holders.
func (s *Slots) UIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.held))
	for u := range s.held {
		out = append(out, u)
	}
	return out
}
