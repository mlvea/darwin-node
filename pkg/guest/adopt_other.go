//go:build !darwin

package guest

// NewHostPlatform is the platform the guest agent installs. Off darwin it
// is the in-memory stand-in, so a Linux test process does not reseed its
// own kernel or rename the machine.
func NewHostPlatform() AdoptPlatform { return NewSoftwarePlatform() }
