package simvz

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

// Start moves Stopped or Error to Starting, then Running. A third live VM
// returns ErrVirtualMachineLimitExceeded asynchronously (completion
// handler versus KVO) and ends in Error without taking a slot. Create
// does not take one either.
func (m *machine) Start(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("simvz: nil machine")
	}
	m.mustOutside()
	if err := ctx.Err(); err != nil {
		return err
	}
	// cleanup drops the machine. A later Start is the same object, so
	// register it again before boot.
	m.rt.track(m)
	if err := m.enterStarting(ctx); err != nil {
		return err
	}
	if err := m.pauseTransition(ctx); err != nil {
		m.failStart(err)
		return err
	}
	if err := m.boot(ctx); err != nil {
		m.failStart(err)
		return err
	}
	immediate := m.faults.CrashGuest && m.faults.CrashAfter <= 0
	if immediate {
		// Still Starting, so a concurrent Stop is refused and cannot
		// Wait the accept loop out from under this crash.
		m.crashGuest()
		m.waitGen()
	}
	if err := m.markRunning(ctx, immediate); err != nil {
		m.failStart(err)
		return err
	}
	if m.faults.CrashGuest && !immediate {
		m.mu.Lock()
		life := m.life
		m.mu.Unlock()
		m.armCrash(life)
	}
	return nil
}

func (m *machine) enterStarting(ctx context.Context) error {
	m.mu.Lock()
	from := m.state
	if from != vzStopped && from != vzError {
		st := from.String()
		m.mu.Unlock()
		err := &StateError{Op: "start", State: st}
		m.recordCompletion(err)
		return err
	}
	m.mu.Unlock()
	if err := m.rt.reserve(); err != nil {
		m.deliverLimit(ctx, err, true)
		return err
	}
	release, err := m.beginBoot(from, vzStarting, "start")
	if err != nil {
		m.rt.release()
		m.recordCompletion(err)
		return err
	}
	m.releaseSlot(release)
	m.barrier(ctx, BarrierBeforeKVOStarting)
	return nil
}

func (m *machine) deliverLimit(ctx context.Context, err error, toError bool) {
	m.q.Do(func() {
		if m.rt.completionBeforeKVO() {
			m.barrier(ctx, BarrierBeforeStartCompletion)
			m.recordCompletion(err)
			if toError {
				m.barrier(ctx, BarrierBeforeKVOError)
				m.become(vzError)
			}
			return
		}
		if toError {
			m.barrier(ctx, BarrierBeforeKVOError)
			m.become(vzError)
		}
		m.barrier(ctx, BarrierBeforeStartCompletion)
		m.recordCompletion(err)
	})
}

func (m *machine) markRunning(ctx context.Context, immediateCrash bool) error {
	var err error
	m.q.Do(func() {
		m.mu.Lock()
		if m.state != vzStarting {
			st := m.state.String()
			m.mu.Unlock()
			err = &StateError{Op: "start", State: st}
			m.recordCompletion(err)
			return
		}
		now := m.clock.Now()
		m.started = &now
		m.finished = nil
		m.agentOK = !immediateCrash
		m.mu.Unlock()
		m.barrier(ctx, BarrierBeforeKVORunning)
		m.become(vzRunning)
		m.barrier(ctx, BarrierBeforeStartCompletion)
		m.recordCompletion(nil)
	})
	return err
}

// Stop force-stops a Running or Paused VM. graceful is wall time: the
// engine passes a real termination grace, and scaling it would hide a
// stuck stop. guestDidStop, the KVO Stopped notification, and the
// completion handler are ordered by Options.Order and can arrive in
// either order. The state Stopping notification is first.
func (m *machine) Stop(ctx context.Context, graceful time.Duration) error {
	if m == nil {
		return fmt.Errorf("simvz: nil machine")
	}
	m.mustOutside()
	var stopping *generation
	m.mu.Lock()
	switch m.state {
	case vzRunning, vzPaused:
		// Capture the boot before Stopped is published. A Start that
		// observes Stopped installs a different generation.
		stopping = m.gen
		release := m.commitLocked(vzStopping)
		m.mu.Unlock()
		m.releaseSlot(release)
		m.barrier(ctx, BarrierBeforeKVOStopping)
	default:
		st := m.state.String()
		m.mu.Unlock()
		err := &StateError{Op: "stop", State: st}
		m.recordCompletion(err)
		return err
	}
	if graceful > 0 {
		waitWall(ctx, graceful)
	}
	_ = m.pauseTransition(ctx)
	steps := m.rt.stopSteps()
	m.q.Do(func() {
		for _, step := range steps {
			switch step {
			case stepGuestStop:
				m.barrier(ctx, BarrierBeforeGuestDidStop)
				m.record(traceEvent{Kind: "delegate_guest_did_stop"})
			case stepKVOStopped:
				m.barrier(ctx, BarrierBeforeKVOStopped)
				now := m.clock.Now()
				m.mu.Lock()
				m.finished = &now
				m.agentOK = false
				m.mu.Unlock()
				m.become(vzStopped)
			case stepStopDone:
				m.barrier(ctx, BarrierBeforeStopCompletion)
				m.recordCompletion(nil)
			}
		}
	})
	m.cleanupGen(ctx, stopping)
	return nil
}

// RequestStop asks a Running guest to stop. Paused is refused: Apple's
// CanRequestStop is false off the running state. The notifications that
// follow are the same ones Stop emits.
func (m *machine) RequestStop(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("simvz: nil machine")
	}
	m.mustOutside()
	m.mu.Lock()
	st := m.state
	m.mu.Unlock()
	if st != vzRunning {
		err := &StateError{Op: "requestStop", State: st.String()}
		m.recordCompletion(err)
		return err
	}
	return m.Stop(ctx, 0)
}

// Pause moves Running through Pausing to Paused. The VM keeps its slot.
// Live vsock fds stay open and stop draining.
func (m *machine) Pause(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("simvz: nil machine")
	}
	m.mustOutside()
	if err := m.cas(vzRunning, vzPausing, "pause"); err != nil {
		m.recordCompletion(err)
		return err
	}
	m.barrier(ctx, BarrierBeforeKVOPausing)
	if err := m.pauseTransition(ctx); err != nil {
		_ = m.cas(vzPausing, vzRunning, "pause")
		m.recordCompletion(err)
		return err
	}
	if err := m.cas(vzPausing, vzPaused, "pause"); err != nil {
		m.recordCompletion(err)
		return err
	}
	m.currentFlow().setPaused(true)
	m.recordCompletion(nil)
	return nil
}

// Resume moves Paused through Resuming back to Running and lets vsock
// data flow again.
func (m *machine) Resume(ctx context.Context) error {
	if m == nil {
		return fmt.Errorf("simvz: nil machine")
	}
	m.mustOutside()
	if err := m.cas(vzPaused, vzResuming, "resume"); err != nil {
		m.recordCompletion(err)
		return err
	}
	m.barrier(ctx, BarrierBeforeKVOResuming)
	if err := m.pauseTransition(ctx); err != nil {
		_ = m.cas(vzResuming, vzPaused, "resume")
		m.recordCompletion(err)
		return err
	}
	if err := m.cas(vzResuming, vzRunning, "resume"); err != nil {
		m.recordCompletion(err)
		return err
	}
	m.currentFlow().setPaused(false)
	m.recordCompletion(nil)
	return nil
}

// Save writes a JSON machine descriptor. The VM must be Paused; any other
// state is VZErrorInvalidVirtualMachineState. On success the VM stays
// Paused and keeps its slot. path empty writes beside the cloned disk,
// which Stop deletes; pass a path outside that directory to Restore later.
func (m *machine) Save(ctx context.Context) error {
	return m.SaveTo(ctx, "")
}

// SaveTo is Save with an explicit file path.
func (m *machine) SaveTo(ctx context.Context, path string) error {
	if m == nil {
		return fmt.Errorf("simvz: nil machine")
	}
	m.mustOutside()
	if err := ctx.Err(); err != nil {
		return err
	}
	if path == "" {
		m.mu.Lock()
		dir := m.dir
		m.mu.Unlock()
		if dir == "" {
			err := fmt.Errorf("simvz: save: VM has no directory")
			m.recordCompletion(err)
			return err
		}
		path = filepath.Join(dir, "machine-state.json")
	}
	if err := m.cas(vzPaused, vzSaving, "save"); err != nil {
		m.recordCompletion(err)
		return err
	}
	m.barrier(ctx, BarrierBeforeKVOSaving)
	if err := m.writeSnapshot(path); err != nil {
		_ = m.cas(vzSaving, vzPaused, "save")
		m.recordCompletion(err)
		return err
	}
	if err := m.pauseTransition(ctx); err != nil {
		_ = m.cas(vzSaving, vzPaused, "save")
		m.recordCompletion(err)
		return err
	}
	if err := m.cas(vzSaving, vzPaused, "save"); err != nil {
		m.recordCompletion(err)
		return err
	}
	m.mu.Lock()
	m.snapshotPath = path
	m.mu.Unlock()
	m.q.Do(func() {
		m.barrier(ctx, BarrierBeforeSaveCompletion)
		m.recordCompletion(nil)
	})
	return nil
}

// Restore loads a save file into a Stopped VM. The descriptor must match
// this machine's id, hardware model, MAC, CPU count, and memory size.
// Success ends in Paused and takes a slot. Failure leaves the state
// unchanged. A third live VM fails with the limit error and does not
// take a slot. Old vsock connections are not carried across.
func (m *machine) Restore(ctx context.Context, path string) error {
	if m == nil {
		return fmt.Errorf("simvz: nil machine")
	}
	m.mustOutside()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	from := m.state
	m.mu.Unlock()
	if from != vzStopped && from != vzError {
		err := &StateError{Op: "restore", State: from.String()}
		m.recordCompletion(err)
		return err
	}
	desc, err := readSnapshot(path)
	if err != nil {
		err = restoreFailed(err)
		m.recordCompletion(err)
		return err
	}
	if !m.matches(desc) {
		err = restoreFailed(fmt.Errorf("machine identity, hardware, MAC, CPU, or memory does not match"))
		m.recordCompletion(err)
		return err
	}
	if err := m.rt.reserve(); err != nil {
		m.deliverLimit(ctx, err, false)
		return err
	}
	release, err := m.beginBoot(from, vzRestoring, "restore")
	if err != nil {
		m.rt.release()
		m.recordCompletion(err)
		return err
	}
	m.releaseSlot(release)
	m.barrier(ctx, BarrierBeforeKVORestoring)
	if err := m.pauseTransition(ctx); err != nil {
		m.failStart(err)
		return err
	}
	m.become(vzPaused)
	m.currentFlow().setPaused(true)
	life := m.startLife()
	if !m.faults.HoldAgent {
		var bootErr error
		if m.rt != nil && m.rt.guestBin != "" {
			bootErr = m.spawnGuest(ctx)
		} else {
			bootErr = m.serveInProcess(life)
		}
		if bootErr != nil {
			m.recordCompletion(bootErr)
			return bootErr
		}
	}
	m.mu.Lock()
	m.snapshotPath = path
	m.mu.Unlock()
	m.q.Do(func() {
		m.barrier(ctx, BarrierBeforeRestoreCompletion)
		m.recordCompletion(nil)
	})
	return nil
}
