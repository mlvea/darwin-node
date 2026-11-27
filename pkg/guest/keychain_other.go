//go:build !darwin

package guest

// NewHostKeychainPlatform is the platform the guest agent installs. Off
// darwin it is the in-memory image, so a Linux test process does not create
// a keychain on the machine running the tests.
func NewHostKeychainPlatform() KeychainPlatform { return NewImageKeychains() }
