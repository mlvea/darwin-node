package vmidentity_test

import (
	"errors"
	"sync"
	"testing"

	"github.com/mlvea/darwin-node/pkg/vmidentity"
)

func TestReseedBarrierAdoptFailsClosed(t *testing.T) {
	var missing *vmidentity.ReseedBarrier
	if err := missing.Adopt(); !errors.Is(err, vmidentity.ErrReseedRequired) {
		t.Fatalf("nil barrier adopt %v, want ErrReseedRequired", err)
	}

	b := vmidentity.NewReseedBarrier()
	if err := b.Adopt(); !errors.Is(err, vmidentity.ErrReseedRequired) {
		t.Fatalf("fresh barrier adopt %v, want ErrReseedRequired", err)
	}
	for _, n := range []int{-1, 0, 1, vmidentity.ReseedBytes - 1} {
		if err := b.NoteGuestReseed(n); !errors.Is(err, vmidentity.ErrShortReseed) {
			t.Fatalf("note %d: %v, want ErrShortReseed", n, err)
		}
		if err := b.Adopt(); !errors.Is(err, vmidentity.ErrReseedRequired) {
			t.Fatalf("adopt after %d bytes: %v, want ErrReseedRequired", n, err)
		}
	}
	// Two short writes are not one reseed. write_random reseeds on a
	// single 64-byte write, and partial writes must not accumulate.
	if err := b.NoteGuestReseed(32); !errors.Is(err, vmidentity.ErrShortReseed) {
		t.Fatal(err)
	}
	if err := b.NoteGuestReseed(32); !errors.Is(err, vmidentity.ErrShortReseed) {
		t.Fatal(err)
	}
	if err := b.Adopt(); !errors.Is(err, vmidentity.ErrReseedRequired) {
		t.Fatalf("adopt after two 32-byte writes: %v", err)
	}
}

func TestReseedBarrierOpensAfterFullWrite(t *testing.T) {
	b := vmidentity.NewReseedBarrier()
	if err := b.NoteGuestReseed(vmidentity.ReseedBytes); err != nil {
		t.Fatal(err)
	}
	if err := b.Adopt(); err != nil {
		t.Fatal(err)
	}
	if err := b.Adopt(); err != nil {
		t.Fatalf("second adopt: %v", err)
	}
	// A later short write must not close the barrier.
	if err := b.NoteGuestReseed(1); !errors.Is(err, vmidentity.ErrShortReseed) {
		t.Fatalf("short write after reseed: %v", err)
	}
	if err := b.Adopt(); err != nil {
		t.Fatalf("adopt after a late short write: %v", err)
	}

	longer := vmidentity.NewReseedBarrier()
	if err := longer.NoteGuestReseed(vmidentity.ReseedBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := longer.Adopt(); err != nil {
		t.Fatal(err)
	}
}

func TestReseedBarrierConcurrentNoteAndAdopt(t *testing.T) {
	b := vmidentity.NewReseedBarrier()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_ = b.NoteGuestReseed(vmidentity.ReseedBytes)
	}()
	go func() {
		defer wg.Done()
		_ = b.Adopt()
	}()
	wg.Wait()
	if err := b.Adopt(); err != nil {
		t.Fatalf("adopt after concurrent reseed: %v", err)
	}
}
