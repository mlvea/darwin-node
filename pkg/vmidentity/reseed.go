package vmidentity

import (
	"errors"
	"sync"
)

// ReseedBytes is the single write that makes XNU write_random reseed the
// kernel CSPRNG. A shorter write does not, and this barrier does not add
// short writes together.
const ReseedBytes = 64

// ErrReseedRequired is returned by Adopt while the guest has not reseeded.
var ErrReseedRequired = errors.New("guest reseed required before adopt")

// ErrShortReseed is returned when a reported /dev/random write is shorter
// than ReseedBytes. The barrier stays where it was.
var ErrShortReseed = errors.New("guest reseed write is shorter than 64 bytes")

// ReseedBarrier is the fail-closed gate for adopting a restored guest.
//
// Virtualization.framework does not publish vmgenid. After a save/restore
// the guest kernel and processes that were already running keep the RNG
// state from the snapshot until the guest writes ReseedBytes to
// /dev/random. Processes exec'd after that write see the new entropy.
// Adopt refuses the guest until that write is recorded. A nil barrier
// refuses too.
//
// Cold boot does not create a barrier. A new boot has a new RNG, and the
// virtio entropy device is attached so a later snapshot pool can reseed.
// That pool is not implemented here; it should call NewReseedBarrier
// after restore and NoteGuestReseed once the guest write has happened.
type ReseedBarrier struct {
	mu   sync.Mutex
	open bool
}

// NewReseedBarrier returns a barrier that rejects Adopt.
func NewReseedBarrier() *ReseedBarrier {
	return &ReseedBarrier{}
}

// NoteGuestReseed records one write to /dev/random. n must be at least
// ReseedBytes or the barrier does not open.
func (b *ReseedBarrier) NoteGuestReseed(n int) error {
	if b == nil {
		return ErrReseedRequired
	}
	if n < ReseedBytes {
		return ErrShortReseed
	}
	b.mu.Lock()
	b.open = true
	b.mu.Unlock()
	return nil
}

// Adopt returns ErrReseedRequired until a full reseed has been recorded.
func (b *ReseedBarrier) Adopt() error {
	if b == nil {
		return ErrReseedRequired
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.open {
		return ErrReseedRequired
	}
	return nil
}
