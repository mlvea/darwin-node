//go:build !darwin

package guest

// HaltGuest is a no-op off Darwin. Linux tests pass their own halt func
// and must not shut the machine down.
func HaltGuest() {}
