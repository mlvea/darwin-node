//go:build darwin

package guest

// The real keychain calls live here so a Mac guest can run them. Linux CI
// and SimVZ do not. Tests must not call this platform: it changes the
// keychains of the Mac it runs on. Hardware-pending: see docs/guest-keychain.md.

/*
#cgo LDFLAGS: -framework Security -framework CoreFoundation
#include <CoreFoundation/CoreFoundation.h>
#include <Security/Security.h>
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"
)

// NewHostKeychainPlatform is the in-guest OS. On darwin it creates and
// unlocks a keychain with Security.framework, so the password is not a
// process argument, and it edits the user search list with the security
// CLI. DARWIN_NODE_GUEST_SOFTWARE_PLATFORM selects the in-memory platform.
// SimVZ sets that variable when it execs this agent. The launchd job inside
// a VM image does not.
func NewHostKeychainPlatform() KeychainPlatform {
	if softwarePlatformRequested() {
		return NewImageKeychains()
	}
	return darwinKeychain{}
}

type darwinKeychain struct{}

func (darwinKeychain) Home() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("workload home")
	}
	return home, nil
}

func (darwinKeychain) Create(path, password string) error {
	if password == "" {
		return errors.New("empty keychain password")
	}
	if !isJobKeychain(path) || isSystemKeychain(path) || isLoginKeychain(path) {
		return errors.New("refusing to create a keychain that is not a job keychain")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return secKeychainCreate(path, password)
}

func (darwinKeychain) Unlock(path, password string, noAutoLock bool) error {
	return secKeychainUnlock(path, password, noAutoLock)
}

func (darwinKeychain) SetDefault(path string) error {
	if isSystemKeychain(path) {
		return errors.New("refusing to make the System keychain the user default")
	}
	_, err := runSecurity("default-keychain", "-d", "user", "-s", path)
	return err
}

func (darwinKeychain) SetUserSearchList(paths []string) error {
	if len(paths) == 0 {
		return errors.New("user search list is empty")
	}
	args := []string{"list-keychains", "-d", "user", "-s"}
	for _, path := range paths {
		if isSystemKeychain(path) {
			return errors.New("refusing to put the System keychain on the user search list")
		}
		args = append(args, path)
	}
	_, err := runSecurity(args...)
	return err
}

func (darwinKeychain) HideLogin(home string) error {
	dir := filepath.Join(home, "Library", "Keychains")
	for _, name := range []string{"login.keychain-db", "login.keychain"} {
		src := filepath.Join(dir, name)
		fi, err := os.Lstat(src)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if fi.IsDir() {
			return fmt.Errorf("login keychain path %s is a directory", src)
		}
		dst := src + loginHiddenSuffix
		if _, err := os.Lstat(dst); err == nil {
			return errors.New("hidden login keychain already exists")
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.Rename(src, dst); err != nil {
			return err
		}
	}
	return nil
}

func (darwinKeychain) Delete(path string) error {
	if isSystemKeychain(path) || isLoginKeychain(path) || !isJobKeychain(path) {
		return errors.New("refusing to delete a keychain that is not a job keychain")
	}
	err := os.Remove(path)
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (darwinKeychain) Default() (string, error) {
	out, err := runSecurity("default-keychain", "-d", "user")
	if err != nil {
		return "", err
	}
	paths := parseSecurityPaths(out)
	if len(paths) != 1 {
		return "", errors.New("default keychain")
	}
	return paths[0], nil
}

func (darwinKeychain) UserSearchList() ([]string, error) {
	out, err := runSecurity("list-keychains", "-d", "user")
	if err != nil {
		return nil, err
	}
	return parseSecurityPaths(out), nil
}

func (darwinKeychain) SystemPath() (string, error) {
	path := filepath.Join("/Library", "Keychains", "System.keychain")
	if _, err := os.Lstat(path); err != nil {
		return "", err
	}
	return path, nil
}

func (darwinKeychain) LoginHidden(home string) (bool, error) {
	dir := filepath.Join(home, "Library", "Keychains")
	for _, name := range []string{"login.keychain-db", "login.keychain"} {
		_, err := os.Lstat(filepath.Join(dir, name))
		if err == nil {
			return false, nil
		}
		if !os.IsNotExist(err) {
			return false, err
		}
	}
	return true, nil
}

func runSecurity(args ...string) (string, error) {
	cmd := exec.Command("/usr/bin/security", args...)
	out, err := cmd.CombinedOutput()
	text := strings.TrimSpace(string(out))
	if err != nil {
		if text == "" {
			return "", err
		}
		return "", fmt.Errorf("%w: %s", err, text)
	}
	return text, nil
}

func parseSecurityPaths(out string) []string {
	var paths []string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		line = strings.Trim(line, `"`)
		if line == "" {
			continue
		}
		paths = append(paths, line)
	}
	return paths
}

func secKeychainCreate(path, password string) error {
	if password == "" {
		return errors.New("empty keychain password")
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	pw, n, wipe, err := cPassword(password)
	if err != nil {
		return err
	}
	defer wipe()
	var kc C.SecKeychainRef
	st := C.SecKeychainCreate(cpath, n, pw, C.Boolean(0), nil, &kc)
	if st != 0 {
		return fmt.Errorf("SecKeychainCreate: %d", int32(st))
	}
	releaseKeychain(kc)
	return nil
}

func secKeychainUnlock(path, password string, noAutoLock bool) error {
	if password == "" {
		return errors.New("empty keychain password")
	}
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var kc C.SecKeychainRef
	if st := C.SecKeychainOpen(cpath, &kc); st != 0 {
		return fmt.Errorf("SecKeychainOpen: %d", int32(st))
	}
	defer releaseKeychain(kc)
	pw, n, wipe, err := cPassword(password)
	if err != nil {
		return err
	}
	defer wipe()
	st := C.SecKeychainUnlock(kc, n, pw, C.Boolean(1))
	if st != 0 {
		return fmt.Errorf("SecKeychainUnlock: %d", int32(st))
	}
	if !noAutoLock {
		return nil
	}
	// SEC_KEYCHAIN_SETTINGS_VERS1 is 1. useLockInterval false and
	// lockOnSleep false is no auto-lock for the job.
	settings := C.SecKeychainSettings{
		version:         1,
		lockOnSleep:     C.Boolean(0),
		useLockInterval: C.Boolean(0),
		lockInterval:    0,
	}
	if st := C.SecKeychainSetSettings(kc, &settings); st != 0 {
		return fmt.Errorf("SecKeychainSetSettings: %d", int32(st))
	}
	return nil
}

// cPassword copies password into C memory so Security.framework does not
// receive a Go pointer. wipe zeros that memory and frees it. The Go string
// is not logged and is not sent on the wire.
func cPassword(password string) (unsafe.Pointer, C.UInt32, func(), error) {
	n := len(password)
	if n == 0 {
		return nil, 0, func() {}, errors.New("empty keychain password")
	}
	buf := C.malloc(C.size_t(n))
	if buf == nil {
		return nil, 0, func() {}, errors.New("keychain password")
	}
	raw := unsafe.Slice((*byte)(buf), n)
	copy(raw, password)
	return buf, C.UInt32(n), func() {
		for i := range raw {
			raw[i] = 0
		}
		C.free(buf)
	}, nil
}

func releaseKeychain(kc C.SecKeychainRef) {
	if kc == nil {
		return
	}
	C.CFRelease(C.CFTypeRef(unsafe.Pointer(kc)))
}
