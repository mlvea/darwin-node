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
	m.bindGenLocked()
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
	m.bindGenLocked()
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
	m.bindGenLocked()
	m.mu.Unlock()
	return nil
}

func (m *machine) startLife() context.Context {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.gen != nil && m.gen.ctx != nil {
		m.life = m.gen.ctx
		m.lifeCancel = m.gen.cancel
		return m.gen.ctx
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.life = ctx
	m.lifeCancel = cancel
	m.bindGenLocked()
	return ctx
}
