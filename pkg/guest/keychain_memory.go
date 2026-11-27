package guest

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
)

// kcFile is one keychain in an in-memory image. passHash is set for a job
// keychain. The password itself is not kept.
type kcFile struct {
	kind       string
	id         string
	passHash   [32]byte
	hasPass    bool
	noAutoLock bool
}

// MemoryKeychain is the keychain directory of one booted image. A fresh
// value has the golden login keychain and the System keychain. Jobs that
// share one value share that login keychain until each installs its own.
type MemoryKeychain struct {
	mu         sync.Mutex
	home       string
	system     string
	def        string
	userSearch []string
	files      map[string]*kcFile
	fail       string
	calls      []string
}

// NewImageKeychains returns the golden image keychains. The login keychain
// is the default and the only user search-list entry. The System keychain
// is present and is not on that list.
func NewImageKeychains() *MemoryKeychain {
	const home = "/Users/runner"
	login := filepath.Join(home, "Library", "Keychains", "login.keychain-db")
	system := filepath.Join("/Library", "Keychains", "System.keychain")
	return &MemoryKeychain{
		home:       home,
		system:     system,
		def:        login,
		userSearch: []string{login},
		files: map[string]*kcFile{
			login:  {kind: "login", id: "image-login"},
			system: {kind: "system", id: "image-system"},
		},
	}
}

// FailAt makes the named install step return an error. The step names are
// home, create, unlock, default, search, hide, and delete. The failure
// happens before that step changes the keychain directory.
func (m *MemoryKeychain) FailAt(step string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.fail = step
	m.mu.Unlock()
}

// Calls is the platform methods Install has reached, in order.
func (m *MemoryKeychain) Calls() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// Exists reports whether path is still in this directory.
func (m *MemoryKeychain) Exists(path string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.files[path]
	return ok
}

// NoAutoLock reports whether path was unlocked with auto-lock disabled.
func (m *MemoryKeychain) NoAutoLock(path string) bool {
	if m == nil {
		return false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	f := m.files[path]
	return f != nil && f.noAutoLock
}

func (m *MemoryKeychain) hit(step string) error {
	m.calls = append(m.calls, step)
	if m.fail == step {
		return fmt.Errorf("%s failed", step)
	}
	return nil
}

func (m *MemoryKeychain) Home() (string, error) {
	if m == nil {
		return "", errors.New("nil keychain")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.hit("home"); err != nil {
		return "", err
	}
	if m.home == "" || !filepath.IsAbs(m.home) {
		return "", errors.New("workload home")
	}
	return m.home, nil
}

func (m *MemoryKeychain) Default() (string, error) {
	if m == nil {
		return "", errors.New("nil keychain")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.def == "" {
		return "", errors.New("default keychain")
	}
	return m.def, nil
}

func (m *MemoryKeychain) UserSearchList() ([]string, error) {
	if m == nil {
		return nil, errors.New("nil keychain")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.userSearch...), nil
}

func (m *MemoryKeychain) SystemPath() (string, error) {
	if m == nil {
		return "", errors.New("nil keychain")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.files[m.system]; !ok {
		return "", errors.New("system keychain missing")
	}
	return m.system, nil
}

func (m *MemoryKeychain) LoginHidden(home string) (bool, error) {
	if m == nil {
		return false, errors.New("nil keychain")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loginHidden(home), nil
}

func (m *MemoryKeychain) loginHidden(home string) bool {
	dir := filepath.Join(home, "Library", "Keychains")
	for _, name := range []string{"login.keychain-db", "login.keychain"} {
		if _, ok := m.files[filepath.Join(dir, name)]; ok {
			return false
		}
	}
	return true
}

func (m *MemoryKeychain) Create(path, password string) error {
	if m == nil {
		return errors.New("nil keychain")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.hit("create"); err != nil {
		return err
	}
	if password == "" {
		return errors.New("empty keychain password")
	}
	if !isJobKeychain(path) || isSystemKeychain(path) || isLoginKeychain(path) {
		return errors.New("refusing to create a keychain that is not a job keychain")
	}
	if old, ok := m.files[path]; ok && old.kind != "job" {
		return errors.New("refusing to replace a keychain that is not a job keychain")
	}
	id, err := newKeychainID()
	if err != nil {
		return err
	}
	m.files[path] = &kcFile{
		kind:     "job",
		id:       id,
		passHash: hashKeychainPassword(password),
		hasPass:  true,
	}
	return nil
}

func (m *MemoryKeychain) Unlock(path, password string, noAutoLock bool) error {
	if m == nil {
		return errors.New("nil keychain")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.hit("unlock"); err != nil {
		return err
	}
	f := m.files[path]
	if f == nil || !f.hasPass || !passwordMatches(f.passHash, password) {
		return errors.New("unlock failed")
	}
	f.noAutoLock = noAutoLock
	return nil
}

func (m *MemoryKeychain) SetDefault(path string) error {
	if m == nil {
		return errors.New("nil keychain")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.hit("default"); err != nil {
		return err
	}
	if isSystemKeychain(path) {
		return errors.New("refusing to make the System keychain the user default")
	}
	if _, ok := m.files[path]; !ok {
		return errors.New("default keychain missing")
	}
	m.def = path
	return nil
}

func (m *MemoryKeychain) SetUserSearchList(paths []string) error {
	if m == nil {
		return errors.New("nil keychain")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.hit("search"); err != nil {
		return err
	}
	if len(paths) == 0 {
		return errors.New("user search list is empty")
	}
	for _, path := range paths {
		if isSystemKeychain(path) {
			return errors.New("refusing to put the System keychain on the user search list")
		}
		if _, ok := m.files[path]; !ok {
			return errors.New("search list keychain missing")
		}
	}
	m.userSearch = append([]string(nil), paths...)
	return nil
}

func (m *MemoryKeychain) HideLogin(home string) error {
	if m == nil {
		return errors.New("nil keychain")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.hit("hide"); err != nil {
		return err
	}
	dir := filepath.Join(home, "Library", "Keychains")
	for _, name := range []string{"login.keychain-db", "login.keychain"} {
		src := filepath.Join(dir, name)
		f, ok := m.files[src]
		if !ok {
			continue
		}
		dst := src + loginHiddenSuffix
		if _, exists := m.files[dst]; exists {
			return errors.New("hidden login keychain already exists")
		}
		delete(m.files, src)
		f.kind = "hidden"
		m.files[dst] = f
		kept := m.userSearch[:0]
		for _, path := range m.userSearch {
			if path != src {
				kept = append(kept, path)
			}
		}
		m.userSearch = kept
	}
	return nil
}

func (m *MemoryKeychain) Delete(path string) error {
	if m == nil {
		return errors.New("nil keychain")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.hit("delete"); err != nil {
		return err
	}
	if isSystemKeychain(path) || isLoginKeychain(path) || !isJobKeychain(path) {
		return errors.New("refusing to delete a keychain that is not a job keychain")
	}
	delete(m.files, path)
	return nil
}

func newKeychainID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
