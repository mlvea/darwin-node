package engine

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/runtime"
)

// serveConsole exposes the pod's serial console on a host-local unix socket:
// <cache-dir>/pods/<uid>/console.sock. `darwin-node console` dials it. The
// socket exists only when --serial-console is enabled and the runtime
// supports it; everything here is best-effort.
func (e *Engine) serveConsole(rec *podRecord) {
	e.serveConsoleAt(rec, func(path string) (net.Listener, error) {
		return net.Listen("unix", path)
	}, os.Chmod)
}

// serveConsoleAt is serveConsole with the socket calls injected so tests can
// force Listen and Chmod failures. Those paths must close the serial pipe.
func (e *Engine) serveConsoleAt(rec *podRecord, listen func(string) (net.Listener, error), chmod func(string, os.FileMode) error) {
	if !e.cfg.SerialConsole {
		return
	}
	rec.mu.Lock()
	machine := rec.machine
	pod := rec.pod
	rec.mu.Unlock()
	if machine == nil || pod == nil {
		return
	}
	cons, ok := machine.(runtime.Consoler)
	if !ok {
		return
	}
	consoleConn, err := cons.Console()
	if err != nil {
		e.events.Warn(context.Background(), event.ReasonFailed, "console: "+err.Error())
		return
	}
	// The serial pipe is a process fd (os.Pipe on the vz runtime). Close it
	// on every return until the hub owns it. Listen and Chmod both fail
	// after Console() has already taken the pipe.
	owned := true
	defer func() {
		if owned {
			_ = consoleConn.Close()
		}
	}()
	sockPath := ConsoleSocketPath(pod.Namespace, pod.Name)
	_ = os.Remove(sockPath)
	ln, err := listen(sockPath)
	if err != nil {
		e.events.Warn(context.Background(), event.ReasonFailed, "console socket: "+err.Error())
		return
	}
	if err := chmod(sockPath, 0o600); err != nil {
		_ = ln.Close()
		_ = os.Remove(sockPath)
		e.events.Warn(context.Background(), event.ReasonFailed, "console socket mode: "+err.Error())
		return
	}
	owned = false
	hub := newConsoleHub(consoleConn)
	rec.mu.Lock()
	rec.consoleLn = ln
	rec.consoleSock = sockPath
	rec.consoleHub = hub
	rec.mu.Unlock()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go hub.attach(c)
		}
	}()
}

// ConsoleSocketPath is where a pod VM's break-glass console is served.
// Deterministic per pod UID so the CLI resolves it without talking to the
// node. Lives in TempDir because unix socket paths are limited to 104 bytes
// on macOS and cache dirs nest deeply.
func ConsoleSocketPath(namespace, name string) string {
	sum := sha256.Sum256([]byte(namespace + "/" + name))
	return filepath.Join(os.TempDir(), fmt.Sprintf("darwin-console-%x.sock", sum[:8]))
}

// consoleHub fans one serial stream out to every attached client. A shared
// io.Copy from the same reader races and delivers each byte to only one of them.
type consoleHub struct {
	console io.ReadWriteCloser

	mu      sync.Mutex
	clients map[net.Conn]struct{}
	closed  bool
	once    sync.Once
}

func newConsoleHub(console io.ReadWriteCloser) *consoleHub {
	return &consoleHub{console: console, clients: map[net.Conn]struct{}{}}
}

func (h *consoleHub) attach(c net.Conn) {
	h.once.Do(func() { go h.readLoop() })
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		_ = c.Close()
		return
	}
	h.clients[c] = struct{}{}
	h.mu.Unlock()

	buf := make([]byte, 4096)
	for {
		n, err := c.Read(buf)
		if n > 0 {
			h.mu.Lock()
			_, werr := h.console.Write(buf[:n])
			closed := h.closed
			h.mu.Unlock()
			if werr != nil || closed {
				break
			}
		}
		if err != nil {
			break
		}
	}
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
	_ = c.Close()
}

func (h *consoleHub) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, err := h.console.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			h.mu.Lock()
			clients := make([]net.Conn, 0, len(h.clients))
			for c := range h.clients {
				clients = append(clients, c)
			}
			h.mu.Unlock()
			for _, c := range clients {
				_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
				if _, werr := c.Write(chunk); werr != nil {
					h.mu.Lock()
					delete(h.clients, c)
					h.mu.Unlock()
					_ = c.Close()
				}
			}
		}
		if err != nil {
			return
		}
	}
}

func (h *consoleHub) close() {
	if h == nil {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	h.closed = true
	clients := h.clients
	h.clients = map[net.Conn]struct{}{}
	console := h.console
	h.mu.Unlock()
	for c := range clients {
		_ = c.Close()
	}
	if console != nil {
		_ = console.Close()
	}
}
