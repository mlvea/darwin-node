package simvz

import (
	"context"

	"github.com/mlvea/darwin-node/pkg/guest"
)

func (m *machine) installKeychain(h *guest.Handler) {
	if m == nil || h == nil || m.rt == nil {
		return
	}
	kc := guest.NewImageKeychains()
	if m.faults.Keychain != "" {
		kc.FailAt(m.faults.Keychain)
	}
	h.KeychainPlatform = kc
	uid := m.id.UID
	h.OnKeychain = func(context.Context, guest.EphemeralKeychainReq) {
		m.rt.trace.noteMethod(uid, guest.MethodEphemeralKeychain)
	}
	m.rt.trace.keepKeychain(uid, kc)
}

func (t *adoptTrace) keepKeychain(uid string, kc *guest.MemoryKeychain) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.keychains == nil {
		t.keychains = map[string]*guest.MemoryKeychain{}
	}
	t.keychains[uid] = kc
}

// Keychain is the in-memory keychain directory of the latest in-process
// guest with uid. The guest-binary path does not fill it. Nil means that
// guest did not install one.
func (r *Runtime) Keychain(uid string) *guest.MemoryKeychain {
	if r == nil {
		return nil
	}
	r.trace.mu.Lock()
	defer r.trace.mu.Unlock()
	return r.trace.keychains[uid]
}
