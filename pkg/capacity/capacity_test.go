package capacity

import (
	"errors"
	"testing"

	"github.com/darwin-node/darwin-node/pkg/types"
)

func TestNewRejectsOutOfRange(t *testing.T) {
	for _, n := range []int{0, 3, -1, types.AppleMaxConcurrentVMs + 1} {
		if _, err := New(n); !errors.Is(err, ErrInvalidMax) {
			t.Fatalf("New(%d): want ErrInvalidMax, got %v", n, err)
		}
	}
	for _, n := range []int{1, 2} {
		if _, err := New(n); err != nil {
			t.Fatalf("New(%d): %v", n, err)
		}
	}
}

func TestFailClosedThirdVM(t *testing.T) {
	s, err := New(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.TryAcquire("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.TryAcquire("b"); err != nil {
		t.Fatal(err)
	}
	if err := s.TryAcquire("c"); !errors.Is(err, ErrVMCapacityExhausted) {
		t.Fatalf("third acquire: %v", err)
	}
	if s.Used() != 2 || !s.Full() {
		t.Fatalf("used=%d full=%v", s.Used(), s.Full())
	}
	// Idempotent re-acquire of a holder does not consume a new slot.
	if err := s.TryAcquire("a"); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
	s.Release("a")
	if s.Full() {
		t.Fatal("expected a free slot after release")
	}
	if err := s.TryAcquire("c"); err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
}

func TestMaxOne(t *testing.T) {
	s, _ := New(1)
	if err := s.TryAcquire("a"); err != nil {
		t.Fatal(err)
	}
	if err := s.TryAcquire("b"); !errors.Is(err, ErrVMCapacityExhausted) {
		t.Fatalf("got %v", err)
	}
}

func TestRebuild(t *testing.T) {
	s, _ := New(2)
	if err := s.Rebuild([]string{"u1", "u2"}); err != nil {
		t.Fatal(err)
	}
	if !s.Holds("u1") || !s.Holds("u2") {
		t.Fatal("rebuild lost uids")
	}
	if err := s.Rebuild([]string{"a", "b", "c"}); !errors.Is(err, ErrVMCapacityExhausted) {
		t.Fatalf("rebuild overflow: %v", err)
	}
}

func TestReleaseMissingIsOK(t *testing.T) {
	s, _ := New(2)
	s.Release("nope")
}

func TestEmptyUID(t *testing.T) {
	s, _ := New(2)
	if err := s.TryAcquire(""); err == nil {
		t.Fatal("expected error")
	}
}
