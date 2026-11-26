package simvz

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
)

func (m *machine) boot(ctx context.Context) error {
	if err := m.cloneDisk(ctx); err != nil {
		return err
	}
	if m.rt.consumeStartFailure() {
		// The fds a successful start would hold are opened first, then
		// closed by failStart. A caller that only checks the error must
		// not be able to leak them.
		if err := m.armFailResources(); err != nil {
			return err
		}
		return errStartFailed
	}
	if err := m.openNIC(); err != nil {
		return err
	}
	life := m.startLife()
	if m.faults.HoldAgent {
		return nil
	}
	if m.rt.guestBin != "" {
		return m.spawnGuest(ctx)
	}
	return m.serveInProcess(life)
}

func (m *machine) cloneDisk(ctx context.Context) error {
	if m.faults.CloneDelay > 0 {
		if err := m.clock.Sleep(ctx, m.faults.CloneDelay); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := os.MkdirTemp(m.rt.workDir, "simvz-vm-")
	if err != nil {
		return err
	}
	dst := filepath.Join(dir, "disk.img")
	if err := copyDisk(m.spec.DiskPath, dst); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	m.mu.Lock()
	m.dir = dir
	m.disk = dst
	m.mu.Unlock()
	return nil
}

func copyDisk(src, dst string) error {
	if src == "" {
		return os.WriteFile(dst, bytes.Repeat([]byte{0x51}, 4096), 0o600)
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func (m *machine) openNIC() error {
	host, guest, err := datagramPair()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.hostNIC = host
	m.guestNIC = guest
	m.mu.Unlock()
	return nil
}

func (m *machine) armFailResources() error {
	if err := m.openNIC(); err != nil {
		return err
	}
	path, ln, err := listenStream()
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.sock = path
	m.ln = ln
	m.mu.Unlock()
	return nil
}

func (m *machine) startLife() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	m.mu.Lock()
	m.life = ctx
	m.lifeCancel = cancel
	m.mu.Unlock()
	return ctx
}

func (m *machine) cancelLife() {
	m.mu.Lock()
	cancel := m.lifeCancel
	m.lifeCancel = nil
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (m *machine) cleanup() {
	m.cancelLife()
	m.mu.Lock()
	ln := m.ln
	m.ln = nil
	host, guestFile := m.hostNIC, m.guestNIC
	m.hostNIC, m.guestNIC = nil, nil
	proc := m.proc
	m.proc = nil
	sock := m.sock
	dir := m.dir
	m.dir = ""
	m.disk = ""
	m.agentReady = false
	m.mu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
	if proc != nil {
		proc.kill()
	}
	// After the listener is closed, before Wait. A Start that observed
	// Stopped can run here. Unarmed barriers return immediately.
	m.barrier(context.Background(), BarrierBeforeAcceptWait)
	m.wg.Wait()
	if host != nil {
		_ = host.Close()
	}
	if guestFile != nil {
		_ = guestFile.Close()
	}
	if sock != "" {
		_ = os.Remove(sock)
	}
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
	m.dropFlows()
	m.q.stop()
	if m.rt != nil {
		m.rt.untrack(m)
	}
}
