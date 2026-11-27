package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/guest"

	corev1 "k8s.io/api/core/v1"
)

// adoptSampleLimit is how many guest RNG samples this process remembers.
// The oldest is dropped past the limit. A sample that is still in the set
// cannot be accepted for another pod on this node.
const adoptSampleLimit = 1024

// envAllowLegacyGuest skips the Adopt barrier when the guest does not
// implement it. Default off. A guest that implements Adopt and fails the
// proof is still rejected. The variable is read from the environment so
// pkg/config/config.go stays untouched.
const envAllowLegacyGuest = "DARWIN_NODE_ALLOW_LEGACY_GUEST"

// adoptError is a permanent start failure. Retrying the same guest image
// will not produce a reseed it cannot prove, so the pod fails instead of
// sitting in BackOff.
type adoptError struct{ err error }

func (e *adoptError) Error() string {
	if e == nil || e.err == nil {
		return "adopt"
	}
	msg := e.err.Error()
	if strings.Contains(msg, "adopt") {
		return msg
	}
	return "adopt: " + msg
}

func (e *adoptError) Unwrap() error { return e.err }

func (e *adoptError) Permanent() bool { return true }

func adoptFail(err error) error {
	if err == nil {
		return nil
	}
	var ae *adoptError
	if errors.As(err, &ae) {
		return err
	}
	return &adoptError{err: err}
}

func legacyGuestAllowed() bool {
	v := strings.TrimSpace(os.Getenv(envAllowLegacyGuest))
	return v == "1" || strings.EqualFold(v, "true")
}

type adoptSamples struct {
	mu    sync.Mutex
	max   int
	order []string
	seen  map[string]struct{}
}

func newAdoptSamples(max int) *adoptSamples {
	if max < 1 {
		max = 1
	}
	return &adoptSamples{max: max, seen: map[string]struct{}{}}
}

func (s *adoptSamples) Add(sample []byte) error {
	if s == nil {
		return errors.New("adopt: sample set missing")
	}
	if len(sample) != guest.AdoptSampleLen {
		return errors.New("adopt: guest rng sample length")
	}
	var acc byte
	for _, c := range sample {
		acc |= c
	}
	if acc == 0 {
		return errors.New("adopt: guest rng sample is zero")
	}
	key := hex.EncodeToString(sample)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.seen[key]; ok {
		return errors.New("adopt: guest rng sample reused")
	}
	if len(s.order) >= s.max {
		delete(s.seen, s.order[0])
		s.order = append([]string(nil), s.order[1:]...)
	}
	s.order = append(s.order, key)
	s.seen[key] = struct{}{}
	return nil
}

// reseedGuest is the barrier between a live agent session and any
// Materialize or Workload call. A refused reseed stops that start. A
// redial of a guest that already passed the barrier does not call it again.
func (e *Engine) reseedGuest(ctx context.Context, cli *guest.Client, pod *corev1.Pod) error {
	if e == nil || cli == nil || pod == nil {
		return adoptFail(errors.New("missing guest session"))
	}
	if !cli.HasCapability(guest.CapAdopt) {
		return e.legacyOrRefuse()
	}
	host, err := adoptHostname(pod)
	if err != nil {
		return adoptFail(err)
	}
	req, err := guest.NewAdoptRequest(string(pod.UID), host, time.Now())
	if err != nil {
		return adoptFail(err)
	}
	budget := e.agentDialBudget()
	actx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	res, err := cli.Adopt(actx, req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ce *guest.CallError
		if errors.As(err, &ce) && ce.Code == "unknown_method" {
			return e.legacyOrRefuse()
		}
		msg := err.Error()
		if errors.As(err, &ce) && ce.Message != "" {
			msg = ce.Message
		}
		return adoptFail(fmt.Errorf("adopt: %s", guest.SanitizeGuestText(msg)))
	}
	sample, err := guest.CheckAdopt(req, res, time.Now())
	if err != nil {
		return adoptFail(err)
	}
	if e.guestSamples == nil {
		e.guestSamples = newAdoptSamples(adoptSampleLimit)
	}
	if err := e.guestSamples.Add(sample); err != nil {
		return adoptFail(err)
	}
	return nil
}

func (e *Engine) legacyOrRefuse() error {
	if legacyGuestAllowed() {
		e.events.Warn(context.Background(), event.ReasonGuestAgent, "guest adopt skipped: "+envAllowLegacyGuest+" is set")
		return nil
	}
	return adoptFail(errors.New("guest agent does not implement Adopt"))
}

func adoptHostname(pod *corev1.Pod) (string, error) {
	if pod == nil {
		return "", errors.New("no pod")
	}
	name := pod.Name
	if pod.Spec.Hostname != "" {
		name = pod.Spec.Hostname
	}
	if err := guest.ValidateHostname(name); err != nil {
		return "", err
	}
	return name, nil
}
