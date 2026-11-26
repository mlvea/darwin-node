package simvz

import (
	"context"
	"net"
	"os"
	"sync"
)

// generation is one boot of a machine (Start or Restore). Stop captures
// the pointer when it enters Stopping, before it publishes Stopped.
// Cleanup waits on that generation's accept loops and closes only its
// file descriptors. A Start that runs after Stopped is published installs
// a different generation, which this cleanup does not join or tear down.
//
// That is Virtualization.framework canStart: Start is legal in Stopped
// or Error, including while the Stop call that published Stopped is still
// draining the previous boot. Start while the state is still Stopping
// returns VZErrorInvalidVirtualMachineState and does not wait.
type generation struct {
	wg     sync.WaitGroup
	ctx    context.Context
	cancel context.CancelFunc

	ln    net.Listener
	proc  *guestProc
	host  *os.File
	guest *os.File
	sock  string
	dir   string
	flow  *flowGroup
}

// beginBoot claims a generation and moves from -> to. The caller has
// already reserved a slot. On a state mismatch the slot is not released
// here; the caller does that. finishMu is held across the claim and
// track so finishIfIdle cannot untrack a boot that just became current.
func (m *machine) beginBoot(from, to vzState, op string) (bool, error) {
	m.finishMu.Lock()
	defer m.finishMu.Unlock()
	m.mu.Lock()
	if m.state != from {
		st := m.state.String()
		m.mu.Unlock()
		return false, &StateError{Op: op, State: st}
	}
	m.held = true
	m.bootErr = nil
	m.installGenLocked()
	release := m.commitLocked(to)
	m.mu.Unlock()
	if m.rt != nil {
		m.rt.track(m)
	}
	return release, nil
}

// installGenLocked publishes an empty boot. Caller holds m.mu.
// Resource mirrors are cleared so they cannot alias the previous
// generation; that generation object still owns the old descriptors.
func (m *machine) installGenLocked() {
	ctx, cancel := context.WithCancel(context.Background())
	g := &generation{
		ctx:    ctx,
		cancel: cancel,
		flow:   newFlowGroup(),
	}
	m.gen = g
	m.life = ctx
	m.lifeCancel = cancel
	m.flow = g.flow
	m.ln = nil
	m.proc = nil
	m.hostNIC = nil
	m.guestNIC = nil
	m.sock = ""
	m.dir = ""
	m.disk = ""
	m.agentReady = false
}

// bindGenLocked copies the current resource mirrors onto the current
// generation. Caller holds m.mu. A generation that is no longer current
// keeps the descriptors it had when it was unbound.
func (m *machine) bindGenLocked() {
	g := m.gen
	if g == nil {
		return
	}
	g.ln = m.ln
	g.proc = m.proc
	g.host = m.hostNIC
	g.guest = m.guestNIC
	g.sock = m.sock
	g.dir = m.dir
	if m.life != nil {
		g.ctx = m.life
	}
	if m.lifeCancel != nil {
		g.cancel = m.lifeCancel
	}
	if m.flow != nil {
		g.flow = m.flow
	}
}

// cleanup drains whatever generation is current. failStart uses this.
// Stop does not: it passes the generation it captured on entry, so a
// Start that has since installed another one is left alone.
func (m *machine) cleanup() {
	m.mu.Lock()
	g := m.gen
	m.mu.Unlock()
	m.cleanupGen(context.Background(), g)
}

// cleanupGen tears down g. A nil generation only finalizes the machine
// when no boot is installed.
func (m *machine) cleanupGen(ctx context.Context, g *generation) {
	if g == nil {
		m.finishIfIdle()
		return
	}
	res := m.detach(g)
	if res.cancel != nil {
		res.cancel()
	}
	if res.ln != nil {
		_ = res.ln.Close()
	}
	if res.proc != nil {
		res.proc.kill()
	}
	// After this generation's listener is closed, before its Wait.
	// Unarmed, the barrier returns immediately. Start may run here and
	// install a new generation; Wait below does not include that one.
	m.barrier(ctx, BarrierBeforeAcceptWait)
	g.wg.Wait()
	if res.host != nil {
		_ = res.host.Close()
	}
	if res.guest != nil {
		_ = res.guest.Close()
	}
	if res.sock != "" {
		_ = os.Remove(res.sock)
	}
	if res.dir != "" {
		_ = os.RemoveAll(res.dir)
	}
	if res.flow != nil {
		res.flow.close()
	}
	m.finishIfIdle()
}

type genResources struct {
	cancel context.CancelFunc
	ln     net.Listener
	proc   *guestProc
	host   *os.File
	guest  *os.File
	sock   string
	dir    string
	flow   *flowGroup
}

// detach drops g from the machine when it is still current and clears
// the mirrors, which still belong to g in that case. A Start that has
// already installed another generation keeps its mirrors: installGen
// swaps the pointer and the mirrors together. g's own pointers stay
// put so the caller can close them after releasing m.mu.
func (m *machine) detach(g *generation) genResources {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := genResources{
		cancel: g.cancel,
		ln:     g.ln,
		proc:   g.proc,
		host:   g.host,
		guest:  g.guest,
		sock:   g.sock,
		dir:    g.dir,
		flow:   g.flow,
	}
	if m.gen != g {
		return res
	}
	m.gen = nil
	m.ln = nil
	m.proc = nil
	m.hostNIC = nil
	m.guestNIC = nil
	m.sock = ""
	m.dir = ""
	m.disk = ""
	m.agentReady = false
	m.life = nil
	m.lifeCancel = nil
	m.flow = nil
	return res
}

// finishIfIdle stops the actor and drops the machine from the runtime
// when no generation is installed. A Start that claimed one waits out
// this critical section, then tracks itself again.
func (m *machine) finishIfIdle() {
	m.finishMu.Lock()
	defer m.finishMu.Unlock()
	m.mu.Lock()
	idle := m.gen == nil
	m.mu.Unlock()
	if !idle {
		return
	}
	m.q.stop()
	if m.rt != nil {
		m.rt.untrack(m)
	}
}

func (m *machine) waitGen() {
	m.mu.Lock()
	g := m.gen
	m.mu.Unlock()
	if g != nil {
		g.wg.Wait()
	}
}
