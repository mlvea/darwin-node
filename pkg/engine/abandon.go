package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/darwin-node/darwin-node/pkg/capacity"
)

const (
	abandonedBootsFile = "abandoned-boots.json"
	abandonedPodKind   = "pod"
	abandonedWarmKind  = "warm"
	defaultReapTimeout = 5 * time.Second
)

// abandonedBoot is one VM whose Create or Start was still running when
// shutdown gave up. id is the pod UID or the warm slot id, which is also
// the cache/pods directory name.
type abandonedBoot struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// SetShutdownBudgetsForTest shortens Close and the bounded reap that follows
// it. Production keeps the 30s close deadline and a 5s reap. This is not a
// CLI flag.
func (e *Engine) SetShutdownBudgetsForTest(close, reap time.Duration) {
	if e == nil {
		return
	}
	if close > 0 {
		e.closeTimeout = close
	}
	if reap > 0 {
		e.reapTimeout = reap
	}
}

// FinishShutdown is what process exit calls. Close returns at its deadline
// while a boot may still be inside Start; this waits a short extra bound
// for that call to stop the VM. If the VM is still in flight, its id is
// written under the cache dir so the next start can delete the bundle
// without pinning a slot.
func (e *Engine) FinishShutdown() error {
	if e == nil {
		return nil
	}
	err := e.Close()
	if rerr := e.reapAbandoned(); rerr != nil {
		return errors.Join(err, rerr)
	}
	return nil
}

func (e *Engine) reapAbandoned() error {
	d := e.reapTimeout
	if d <= 0 {
		d = defaultReapTimeout
	}
	deadline := time.Now().Add(d)
	for {
		if e.flightCount() == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	boots := e.snapshotAbandoned()
	if len(boots) == 0 {
		return nil
	}
	if err := writeAbandonedBoots(e.cfg.CacheDir, boots); err != nil {
		return err
	}
	ids := make([]string, len(boots))
	for i, b := range boots {
		ids[i] = b.ID
		slog.Error("abandoned boot recorded", "id", b.ID, "kind", b.Kind, "ref", b.Ref)
	}
	return fmt.Errorf("abandoned boot still in flight: %s", strings.Join(ids, ", "))
}

func (e *Engine) beginFlight() {
	e.abandonedMu.Lock()
	e.flights++
	e.abandonedMu.Unlock()
}

func (e *Engine) endFlight(boot abandonedBoot, drop bool) {
	e.abandonedMu.Lock()
	defer e.abandonedMu.Unlock()
	if e.flights > 0 {
		e.flights--
	}
	if !drop {
		return
	}
	if e.settled == nil {
		e.settled = map[string]struct{}{}
	}
	e.settled[boot.ID] = struct{}{}
	e.dropAbandonedLocked(boot.ID)
}

func (e *Engine) flightCount() int {
	e.abandonedMu.Lock()
	defer e.abandonedMu.Unlock()
	return e.flights
}

func (e *Engine) trackBoot(b abandonedBoot) {
	if b.ID == "" {
		return
	}
	e.abandonedMu.Lock()
	defer e.abandonedMu.Unlock()
	if e.inflight == nil {
		e.inflight = map[string]abandonedBoot{}
	}
	e.inflight[b.ID] = b
}

func (e *Engine) untrackBoot(id string) {
	e.abandonedMu.Lock()
	defer e.abandonedMu.Unlock()
	delete(e.inflight, id)
	e.dropAbandonedLocked(id)
}

func (e *Engine) noteAbandoned(b abandonedBoot) {
	e.abandonedMu.Lock()
	defer e.abandonedMu.Unlock()
	e.addAbandonedLocked(b)
}

func (e *Engine) noteInflightWarm() {
	e.abandonedMu.Lock()
	defer e.abandonedMu.Unlock()
	for _, b := range e.inflight {
		if b.Kind == abandonedWarmKind {
			e.addAbandonedLocked(b)
		}
	}
}

func (e *Engine) addAbandonedLocked(b abandonedBoot) {
	if b.ID == "" {
		return
	}
	if _, ok := e.settled[b.ID]; ok {
		return
	}
	for _, have := range e.abandoned {
		if have.ID == b.ID {
			return
		}
	}
	e.abandoned = append(e.abandoned, b)
}

func (e *Engine) dropAbandonedLocked(id string) {
	if len(e.abandoned) == 0 {
		return
	}
	out := e.abandoned[:0]
	for _, b := range e.abandoned {
		if b.ID != id {
			out = append(out, b)
		}
	}
	e.abandoned = out
}

func (e *Engine) snapshotAbandoned() []abandonedBoot {
	e.abandonedMu.Lock()
	defer e.abandonedMu.Unlock()
	if e.flights == 0 {
		return nil
	}
	seen := make(map[string]abandonedBoot, len(e.abandoned)+len(e.inflight))
	for _, b := range e.abandoned {
		if _, ok := e.settled[b.ID]; ok {
			continue
		}
		seen[b.ID] = b
	}
	for id, b := range e.inflight {
		if _, ok := e.settled[id]; ok {
			continue
		}
		seen[id] = b
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]abandonedBoot, 0, len(seen))
	for _, b := range seen {
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func writeAbandonedBoots(cacheDir string, boots []abandonedBoot) error {
	if cacheDir == "" {
		return errors.New("abandoned boot record: cache dir is empty")
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return err
	}
	sort.Slice(boots, func(i, j int) bool { return boots[i].ID < boots[j].ID })
	body, err := json.MarshalIndent(boots, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(filepath.Join(cacheDir, abandonedBootsFile), body, 0o600)
}

// consumeAbandonedBoots deletes bundles named by a previous process and
// then the record. A slot this process already holds is left in place.
// An id that is not a single path segment is skipped so the record cannot
// point RemoveAll outside cache/pods.
func consumeAbandonedBoots(cacheDir string, slots *capacity.Slots) error {
	if cacheDir == "" {
		return nil
	}
	path := filepath.Join(cacheDir, abandonedBootsFile)
	body, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var boots []abandonedBoot
	if err := json.Unmarshal(body, &boots); err != nil {
		return err
	}
	for _, b := range boots {
		if err := safeSegment("boot id", b.ID); err != nil {
			slog.Error("abandoned boot id skipped", "id", b.ID, "error", err)
			continue
		}
		if slots != nil && slots.Holds(b.ID) {
			continue
		}
		dir := filepath.Join(cacheDir, "pods", b.ID)
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		slog.Info("reclaimed abandoned boot", "id", b.ID, "kind", b.Kind, "ref", b.Ref)
	}
	return os.Remove(path)
}
