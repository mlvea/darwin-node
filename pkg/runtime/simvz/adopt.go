package simvz

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/guest"
)

// simAdoptUUID is a fixed platform UUID for injected Adopt responses.
// Honest reseeds use a fresh software-platform UUID instead.
const simAdoptUUID = "11111111-2222-4333-8444-555555555555"

// duplicateSample is the sample every "duplicate" pod returns. The first
// pod on a node is accepted. The next one fails the host's sample set.
var duplicateSample = bytes.Repeat([]byte{0x5a}, guest.AdoptSampleLen)

// AdoptRecord is one guest reseed the in-process agent answered.
type AdoptRecord struct {
	UID     string
	Entropy []byte
	Sample  []byte
}

type adoptTrace struct {
	mu        sync.Mutex
	events    []AdoptRecord
	methods   map[string][]string
	workloads map[string]int
}

func (t *adoptTrace) noteMethod(uid, method string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.methods == nil {
		t.methods = map[string][]string{}
	}
	t.methods[uid] = append(t.methods[uid], method)
	if method == guest.MethodWorkload {
		if t.workloads == nil {
			t.workloads = map[string]int{}
		}
		t.workloads[uid]++
	}
}

func (t *adoptTrace) noteAdopt(uid string, entropy, sample []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.events = append(t.events, AdoptRecord{
		UID:     uid,
		Entropy: append([]byte(nil), entropy...),
		Sample:  append([]byte(nil), sample...),
	})
}

// AdoptEvents is a copy of every Adopt the in-process guests answered.
func (r *Runtime) AdoptEvents() []AdoptRecord {
	if r == nil {
		return nil
	}
	r.trace.mu.Lock()
	defer r.trace.mu.Unlock()
	out := make([]AdoptRecord, len(r.trace.events))
	for i, ev := range r.trace.events {
		out[i] = AdoptRecord{
			UID:     ev.UID,
			Entropy: append([]byte(nil), ev.Entropy...),
			Sample:  append([]byte(nil), ev.Sample...),
		}
	}
	return out
}

// WorkloadCalls is how many Workload RPCs the in-process guest served for uid.
func (r *Runtime) WorkloadCalls(uid string) int {
	if r == nil {
		return 0
	}
	r.trace.mu.Lock()
	defer r.trace.mu.Unlock()
	return r.trace.workloads[uid]
}

// Methods is the in-process RPC trace for uid, in arrival order.
func (r *Runtime) Methods(uid string) []string {
	if r == nil {
		return nil
	}
	r.trace.mu.Lock()
	defer r.trace.mu.Unlock()
	return append([]string(nil), r.trace.methods[uid]...)
}

// VMState is the framework state name of the latest machine with uid.
// "absent" means Create never recorded one.
func (r *Runtime) VMState(uid string) string {
	if r == nil {
		return "absent"
	}
	r.mu.Lock()
	var found *machine
	for _, m := range r.machines {
		if m != nil && m.id.UID == uid {
			found = m
		}
	}
	r.mu.Unlock()
	if found == nil {
		return "absent"
	}
	return found.State()
}

func (m *machine) installAdopt(h *guest.Handler) {
	if m == nil || h == nil {
		return
	}
	uid := m.id.UID
	// Count the RPC and still launch the process. Replacing WorkloadFn
	// would swallow the exit that kubelet conformance waits for.
	h.OnWorkload = func(context.Context, guest.WorkloadReq) {
		m.rt.trace.noteMethod(uid, guest.MethodWorkload)
	}
	h.AdoptFn = func(_ context.Context, req guest.AdoptReq) (guest.AdoptRes, error) {
		m.rt.trace.noteMethod(uid, guest.MethodAdopt)
		switch m.faults.Adopt {
		case "error":
			return guest.AdoptRes{}, errors.New("injected adopt failure")
		case "zero":
			return injectedAdopt(m, uid, req, bytes.Repeat([]byte{0}, guest.AdoptSampleLen))
		case "duplicate":
			return injectedAdopt(m, uid, req, duplicateSample)
		default:
			res, err := guest.PerformAdopt(req, guest.NewSoftwarePlatform())
			if err != nil {
				return guest.AdoptRes{}, err
			}
			sample, decErr := hex.DecodeString(res.GuestRNGSampleHex)
			if decErr != nil {
				return guest.AdoptRes{}, decErr
			}
			m.rt.trace.noteAdopt(uid, req.HostEntropy, sample)
			return res, nil
		}
	}
}

func injectedAdopt(m *machine, uid string, req guest.AdoptReq, sample []byte) (guest.AdoptRes, error) {
	res, err := guest.AcceptedAdopt(req, sample, simAdoptUUID)
	if err != nil {
		return guest.AdoptRes{}, err
	}
	// The host compares this clock with its own wall clock. The request
	// timestamp is already in the past by the time the response is checked.
	res.ClockUnixNano = time.Now().UnixNano()
	m.rt.trace.noteAdopt(uid, req.HostEntropy, sample)
	return res, nil
}
