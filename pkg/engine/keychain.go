package engine

import (
	"context"
	"errors"
	"strings"

	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"

	corev1 "k8s.io/api/core/v1"
)

// keychainError is a permanent start failure. A guest that cannot install
// a fresh keychain will not do it on a retry of the same image, so the pod
// fails instead of sitting in BackOff.
type keychainError struct{ err error }

func (e *keychainError) Error() string {
	if e == nil || e.err == nil {
		return "ephemeral keychain"
	}
	msg := e.err.Error()
	if strings.Contains(msg, "keychain") {
		return msg
	}
	return "ephemeral keychain: " + msg
}

func (e *keychainError) Unwrap() error { return e.err }

func (e *keychainError) Permanent() bool { return true }

func keychainFail(err error) error {
	if err == nil {
		return nil
	}
	var ke *keychainError
	if errors.As(err, &ke) {
		return err
	}
	return &keychainError{err: err}
}

// prepareEphemeralKeychain runs after Adopt and before Workload. A guest
// that does not implement the capability fails the pod unless
// DARWIN_NODE_ALLOW_LEGACY_GUEST is set. Any other error fails the pod.
// The keychain password is never sent and is not part of this error text.
func (e *Engine) prepareEphemeralKeychain(ctx context.Context, cli *guest.Client, pod *corev1.Pod) error {
	if e == nil || cli == nil || pod == nil {
		return keychainFail(errors.New("missing guest session"))
	}
	if !cli.HasCapability(guest.CapEphemeralKeychain) {
		return e.legacyOrRefuseKeychain()
	}
	uid := string(pod.UID)
	budget := e.agentDialBudget()
	actx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	res, err := cli.EphemeralKeychain(actx, guest.EphemeralKeychainReq{PodUID: uid})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ce *guest.CallError
		if errors.As(err, &ce) && ce.Code == "unknown_method" {
			return e.legacyOrRefuseKeychain()
		}
		msg := err.Error()
		if errors.As(err, &ce) && ce.Message != "" {
			msg = ce.Message
		}
		msg = guest.SanitizeGuestText(msg)
		if msg == "" {
			msg = "ephemeral keychain failed"
		}
		return keychainFail(errors.New(msg))
	}
	if err := guest.CheckEphemeralKeychain(uid, res); err != nil {
		return keychainFail(err)
	}
	return nil
}

func (e *Engine) legacyOrRefuseKeychain() error {
	if legacyGuestAllowed() {
		e.events.Warn(context.Background(), event.ReasonGuestAgent, "guest ephemeral keychain skipped: "+envAllowLegacyGuest+" is set")
		return nil
	}
	return keychainFail(errors.New("guest agent does not implement ephemeral keychain"))
}
