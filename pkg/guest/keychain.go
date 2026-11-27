package guest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

const (
	// CapEphemeralKeychain is declared in caps.go. The filename is the
	// contract the host checks: the guest builds it and does not accept a
	// path from the host.
	jobKeychainPrefix = "darwin-node-job-"
	jobKeychainSuffix = ".keychain-db"
	loginHiddenSuffix = ".darwin-node-hidden"
)

// EphemeralKeychainReq asks the guest to install a per-job keychain before
// the workload starts. The password is generated in the guest. It is not a
// field: a value stuffed into the JSON is ignored, and nothing in this
// struct is the password.
type EphemeralKeychainReq struct {
	PodUID string `json:"pod_uid"`
}

// EphemeralKeychainRes is the guest's report of the keychain it installed.
// The password is not a field and is not copied into any of these strings.
type EphemeralKeychainRes struct {
	OK              bool     `json:"ok"`
	KeychainPath    string   `json:"keychain_path"`
	DefaultKeychain string   `json:"default_keychain"`
	UserSearchList  []string `json:"user_search_list"`
	LoginHidden     bool     `json:"login_hidden"`
	SystemKeychain  string   `json:"system_keychain"`
}

// KeychainPlatform is the OS the job keychain touches. Linux tests, SimVZ,
// and the fake runtime use an in-memory platform. The darwin guest agent
// uses the Security framework and the security CLI. A nil platform is not
// used: InstallEphemeralKeychain refuses it.
type KeychainPlatform interface {
	Home() (string, error)
	Default() (string, error)
	UserSearchList() ([]string, error)
	SystemPath() (string, error)
	LoginHidden(home string) (bool, error)
	Create(path, password string) error
	Unlock(path, password string, noAutoLock bool) error
	SetDefault(path string) error
	SetUserSearchList(paths []string) error
	HideLogin(home string) error
	Delete(path string) error
}

// keychainPasswordSource lets a test supply the password the guest would
// have drawn. The production platform does not implement it, so the guest
// draws from crypto/rand. The password still never leaves Install.
type keychainPasswordSource interface {
	KeychainPassword() (string, error)
}

func keychainPassword(p KeychainPlatform) (string, error) {
	if s, ok := p.(keychainPasswordSource); ok {
		pw, err := s.KeychainPassword()
		if err != nil {
			return "", err
		}
		if pw == "" {
			return "", errors.New("empty keychain password")
		}
		return pw, nil
	}
	return randomKeychainPassword()
}

func randomKeychainPassword() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	pw := hex.EncodeToString(buf[:])
	for i := range buf {
		buf[i] = 0
	}
	return pw, nil
}

func hashKeychainPassword(password string) [32]byte {
	return sha256.Sum256([]byte(password))
}

func passwordMatches(sum [32]byte, password string) bool {
	got := hashKeychainPassword(password)
	return subtle.ConstantTimeCompare(sum[:], got[:]) == 1
}

// validateJobPodUID rejects a pod UID that cannot be a single keychain
// filename. Validation finishes before any OS call.
func validateJobPodUID(uid string) error {
	if err := validatePodUID(uid); err != nil {
		return fmt.Errorf("ephemeral keychain: %w", err)
	}
	if uid == "." || uid == ".." || strings.Contains(uid, "..") || strings.ContainsAny(uid, `/\`) {
		return errors.New("ephemeral keychain: pod uid is not a single path component")
	}
	return nil
}

func jobKeychainFilename(uid string) string {
	return jobKeychainPrefix + uid + jobKeychainSuffix
}

func isJobKeychain(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, jobKeychainPrefix) && strings.HasSuffix(base, jobKeychainSuffix) && !strings.Contains(base, "..")
}

func isLoginKeychain(path string) bool {
	base := filepath.Base(path)
	return base == "login.keychain" || base == "login.keychain-db"
}

func isSystemKeychain(path string) bool {
	if filepath.Clean(path) == filepath.Join("/Library", "Keychains", "System.keychain") {
		return true
	}
	base := filepath.Base(path)
	return base == "System.keychain" || base == "System.keychain-db"
}

func jobKeychainPath(home, uid string) (string, error) {
	if home == "" || !filepath.IsAbs(home) {
		return "", errors.New("ephemeral keychain: home")
	}
	path := filepath.Join(home, "Library", "Keychains", jobKeychainFilename(uid))
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", errors.New("ephemeral keychain: path escapes the home directory")
	}
	if !isJobKeychain(path) {
		return "", errors.New("ephemeral keychain: path")
	}
	return path, nil
}

// InstallEphemeralKeychain creates the job keychain, makes it the default
// and the only user search-list entry, unlocks it with no auto-lock, and
// moves the image login keychain out of its well-known name. The System
// keychain is not deleted. The password is not returned. Any error stops
// the sequence.
func InstallEphemeralKeychain(req EphemeralKeychainReq, p KeychainPlatform) (EphemeralKeychainRes, error) {
	if err := validateJobPodUID(req.PodUID); err != nil {
		return EphemeralKeychainRes{}, err
	}
	if p == nil {
		return EphemeralKeychainRes{}, errors.New("ephemeral keychain: platform is nil")
	}
	password, err := keychainPassword(p)
	if err != nil {
		return EphemeralKeychainRes{}, fmt.Errorf("ephemeral keychain: password: %w", err)
	}
	home, err := p.Home()
	if err != nil {
		return EphemeralKeychainRes{}, fmt.Errorf("ephemeral keychain: home: %w", err)
	}
	path, err := jobKeychainPath(home, req.PodUID)
	if err != nil {
		return EphemeralKeychainRes{}, err
	}
	if err := p.Create(path, password); err != nil {
		return EphemeralKeychainRes{}, fmt.Errorf("ephemeral keychain: create: %w", err)
	}
	if err := p.Unlock(path, password, true); err != nil {
		return EphemeralKeychainRes{}, fmt.Errorf("ephemeral keychain: unlock: %w", err)
	}
	if err := p.SetDefault(path); err != nil {
		return EphemeralKeychainRes{}, fmt.Errorf("ephemeral keychain: default: %w", err)
	}
	if err := p.SetUserSearchList([]string{path}); err != nil {
		return EphemeralKeychainRes{}, fmt.Errorf("ephemeral keychain: search list: %w", err)
	}
	if err := p.HideLogin(home); err != nil {
		return EphemeralKeychainRes{}, fmt.Errorf("ephemeral keychain: hide login: %w", err)
	}
	return inspectKeychain(p, home, path)
}

func inspectKeychain(p KeychainPlatform, home, path string) (EphemeralKeychainRes, error) {
	def, err := p.Default()
	if err != nil {
		return EphemeralKeychainRes{}, fmt.Errorf("ephemeral keychain: default: %w", err)
	}
	list, err := p.UserSearchList()
	if err != nil {
		return EphemeralKeychainRes{}, fmt.Errorf("ephemeral keychain: search list: %w", err)
	}
	hidden, err := p.LoginHidden(home)
	if err != nil {
		return EphemeralKeychainRes{}, fmt.Errorf("ephemeral keychain: hide login: %w", err)
	}
	system, err := p.SystemPath()
	if err != nil {
		return EphemeralKeychainRes{}, fmt.Errorf("ephemeral keychain: system keychain: %w", err)
	}
	res := EphemeralKeychainRes{
		OK:              true,
		KeychainPath:    path,
		DefaultKeychain: def,
		UserSearchList:  append([]string(nil), list...),
		LoginHidden:     hidden,
		SystemKeychain:  system,
	}
	if err := CheckEphemeralKeychain(podUIDFromPath(path), res); err != nil {
		return EphemeralKeychainRes{}, err
	}
	return res, nil
}

func podUIDFromPath(path string) string {
	base := filepath.Base(path)
	base = strings.TrimSuffix(base, jobKeychainSuffix)
	return strings.TrimPrefix(base, jobKeychainPrefix)
}

// CheckEphemeralKeychain is the host's view of a response. It rejects a
// guest that did not make the job keychain the only user keychain, or that
// reports the System keychain as that file. The password is not part of
// the check: the guest never sends one.
func CheckEphemeralKeychain(podUID string, res EphemeralKeychainRes) error {
	if err := validateJobPodUID(podUID); err != nil {
		return err
	}
	if !res.OK {
		return errors.New("ephemeral keychain: guest refused")
	}
	name := jobKeychainFilename(podUID)
	if filepath.Base(res.KeychainPath) != name {
		return errors.New("ephemeral keychain: path")
	}
	if res.DefaultKeychain != res.KeychainPath {
		return errors.New("ephemeral keychain: default is not the job keychain")
	}
	if len(res.UserSearchList) != 1 || res.UserSearchList[0] != res.KeychainPath {
		return errors.New("ephemeral keychain: user search list")
	}
	if !res.LoginHidden {
		return errors.New("ephemeral keychain: login keychain still visible")
	}
	if res.SystemKeychain == "" || res.SystemKeychain == res.KeychainPath || isJobKeychain(res.SystemKeychain) || isLoginKeychain(res.SystemKeychain) {
		return errors.New("ephemeral keychain: system keychain")
	}
	return nil
}

// keychainState remembers the job keychain for this agent process so a
// later connection can delete it. The password is not stored.
type keychainState struct {
	mu   sync.Mutex
	p    KeychainPlatform
	path string
}

func newKeychainState(p KeychainPlatform) *keychainState {
	if p == nil {
		p = NewImageKeychains()
	}
	return &keychainState{p: p}
}

func (s *keychainState) install(req EphemeralKeychainReq) (EphemeralKeychainRes, error) {
	if s == nil || s.p == nil {
		return EphemeralKeychainRes{}, errors.New("ephemeral keychain: platform is nil")
	}
	res, err := InstallEphemeralKeychain(req, s.p)
	if err != nil {
		return EphemeralKeychainRes{}, err
	}
	s.mu.Lock()
	s.path = res.KeychainPath
	s.mu.Unlock()
	return res, nil
}

func (s *keychainState) release() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	path := s.path
	p := s.p
	s.path = ""
	s.mu.Unlock()
	if path == "" || p == nil {
		return nil
	}
	if err := p.Delete(path); err != nil {
		s.mu.Lock()
		if s.path == "" {
			s.path = path
		}
		s.mu.Unlock()
		return err
	}
	return nil
}

func (h *Handler) keyState() *keychainState {
	if h == nil {
		return newKeychainState(nil)
	}
	h.Init()
	st := newKeychainState(h.KeychainPlatform)
	if h.live == nil {
		return st
	}
	if cur := h.live.keys.Load(); cur != nil {
		return cur
	}
	if h.live.keys.CompareAndSwap(nil, st) {
		return st
	}
	return h.live.keys.Load()
}

func (h *Handler) ephemeralKeychain(ctx context.Context, req EphemeralKeychainReq) (EphemeralKeychainRes, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return EphemeralKeychainRes{}, err
		}
	}
	return h.keyState().install(req)
}

// releaseJobKeychain deletes the job keychain installed on this agent.
// A guest that never installed one returns nil. The password is not logged.
func (h *Handler) releaseJobKeychain() error {
	if h == nil || h.live == nil {
		return nil
	}
	st := h.live.keys.Load()
	if st == nil {
		return nil
	}
	return st.release()
}

// EphemeralKeychain asks the guest to install the per-job keychain. The
// caller checks the response with CheckEphemeralKeychain.
func (c *Client) EphemeralKeychain(ctx context.Context, req EphemeralKeychainReq) (EphemeralKeychainRes, error) {
	if c == nil || c.sess == nil {
		return EphemeralKeychainRes{}, errors.New("ephemeral keychain: no session")
	}
	env, err := c.sess.Call(ctx, MethodEphemeralKeychain, req)
	if err != nil {
		return EphemeralKeychainRes{}, err
	}
	return Decode[EphemeralKeychainRes](env)
}
