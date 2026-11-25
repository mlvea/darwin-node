package engine

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"

	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/guest"
	"github.com/mlvea/darwin-node/pkg/runtime"

	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	"github.com/virtual-kubelet/virtual-kubelet/node/api"
)

// ExecInVM runs a command in container[0] via the guest agent.
func (e *Engine) ExecInVM(ctx context.Context, namespace, name string, cmd []string, attach api.AttachIO) error {
	e.mu.RLock()
	rec, ok := e.pods[Key(namespace, name)]
	e.mu.RUnlock()
	if !ok {
		return errdefs.NotFound("pod not found")
	}
	rec.mu.Lock()
	cli := rec.agent
	rec.mu.Unlock()
	if cli == nil {
		if !SSHFallbackEnabled(e.cfg) {
			return fmt.Errorf("guest agent not connected")
		}
		ip := ""
		rec.mu.Lock()
		if rec.pod != nil {
			ip = rec.pod.Status.PodIP
		}
		rec.mu.Unlock()
		opts := sshOptsFromConfig(e.cfg, ip)
		if opts.KnownHostsPath == "" && opts.HostKey == nil {
			return fmt.Errorf("ssh fallback requires known_hosts or a pinned host key")
		}
		stdout, stderr, code, err := guest.SSHExec(ctx, opts, cmd)
		if attach != nil {
			if attach.Stdout() != nil {
				_, _ = attach.Stdout().Write(stdout)
			}
			if attach.Stderr() != nil {
				_, _ = attach.Stderr().Write(stderr)
			}
		}
		if err != nil {
			return err
		}
		return commandExit(code)
	}
	req := guest.ExecReq{Argv: cmd, TTY: attach != nil && attach.TTY()}
	var stdin io.Reader
	var resize chan guest.TtyResize
	if attach != nil && attach.Stdin() != nil {
		stdin = attach.Stdin()
	}
	if attach != nil {
		if rc := attach.Resize(); rc != nil {
			resize = make(chan guest.TtyResize, 4)
			go func() {
				for sz := range rc {
					select {
					case resize <- guest.TtyResize{Cols: int(sz.Width), Rows: int(sz.Height)}:
					case <-ctx.Done():
						return
					}
				}
			}()
		}
	}
	if stdin != nil || resize != nil || (attach != nil && (attach.Stdout() != nil || attach.Stderr() != nil)) {
		var stdout, stderr io.Writer
		if attach.Stdout() != nil {
			stdout = attach.Stdout()
		}
		if attach.Stderr() != nil {
			stderr = attach.Stderr()
		}
		code, err := cli.ExecInteractive(ctx, req, stdin, resize, stdout, stderr)
		if err != nil {
			return err
		}
		return commandExit(code)
	}
	_, _, code, err := cli.Exec(ctx, req)
	if err != nil {
		return err
	}
	return commandExit(code)
}

// execExitError is the exit status virtual-kubelet maps onto the exec
// stream. remotecommand type-asserts k8s.io/utils/exec.ExitError, so the
// methods match that interface. A wrapped error would not be mapped.
type execExitError struct{ code int }

func (e execExitError) Error() string   { return fmt.Sprintf("exit code %d", e.code) }
func (e execExitError) String() string  { return e.Error() }
func (e execExitError) Exited() bool    { return true }
func (e execExitError) ExitStatus() int { return e.code }

func commandExit(code int) error {
	if code == 0 {
		return nil
	}
	return execExitError{code: code}
}

// LogsVM streams VM logs via the guest agent.
func (e *Engine) LogsVM(ctx context.Context, namespace, name string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	e.mu.RLock()
	rec, ok := e.pods[Key(namespace, name)]
	e.mu.RUnlock()
	if !ok {
		return nil, errdefs.NotFound("pod not found")
	}
	rec.mu.Lock()
	cli := rec.agent
	machine := rec.machine
	rec.mu.Unlock()
	if cli != nil {
		if opts.Follow {
			// True follow: stream lines into the pipe as the guest appends them.
			r, w := io.Pipe()
			go func() {
				fctx, cancel := context.WithCancel(ctx)
				defer cancel()
				ferr := cli.LogsFollow(fctx, guest.LogsReq{TailLines: opts.Tail, Follow: true, Timestamps: opts.Timestamps}, w)
				if ferr != nil && ferr != context.Canceled && ctx.Err() == nil {
					_ = w.CloseWithError(ferr)
					return
				}
				_ = w.Close()
			}()
			return r, nil
		}
		lines, err := cli.Logs(ctx, guest.LogsReq{TailLines: opts.Tail, Timestamps: opts.Timestamps})
		if err != nil {
			return nil, err
		}
		r, w := io.Pipe()
		go func() {
			defer w.Close()
			for _, ln := range lines {
				_, _ = w.Write(ln)
				if len(ln) == 0 || ln[len(ln)-1] != '\n' {
					_, _ = w.Write([]byte("\n"))
				}
			}
		}()
		return r, nil
	}
	if machine != nil {
		return machine.Logs(), nil
	}
	return nil, fmt.Errorf("no logs available")
}

// SidecarLogs / ExecInSidecar are used by the provider for containers[1..].
func (e *Engine) SidecarLogs(ctx context.Context, ns, name, container string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	return e.sidecar.Logs(ctx, ns, name, container, opts)
}

func (e *Engine) ExecInSidecar(ctx context.Context, ns, name, container string, cmd []string, attach api.AttachIO) error {
	return e.sidecar.Exec(ctx, ns, name, container, cmd, attach)
}

// Console attaches to the pod VM's break-glass serial console. Requires a
// runtime whose machines implement runtime.Consoler (vz with
// --serial-console; the fake runtime in tests).
func (e *Engine) Console(namespace, name string) (io.ReadWriteCloser, error) {
	e.mu.RLock()
	rec, ok := e.pods[Key(namespace, name)]
	e.mu.RUnlock()
	if !ok {
		return nil, errdefs.NotFound("pod not found")
	}
	rec.mu.Lock()
	machine := rec.machine
	rec.mu.Unlock()
	if machine == nil {
		return nil, fmt.Errorf("VM not started")
	}
	cons, ok := machine.(runtime.Consoler)
	if !ok {
		return nil, fmt.Errorf("runtime %s does not support serial consoles", e.rt.Name())
	}
	return cons.Console()
}

func decodeSSHKeyB64(s string) []byte {
	if s == "" {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return b
}

// SSHFallbackEnabled reports whether ExecInVM may use SSH when the guest agent is down.
func SSHFallbackEnabled(cfg config.Config) bool {
	return cfg.EnableSSHFallback && cfg.SSHUser != ""
}

func sshOptsFromConfig(cfg config.Config, host string) guest.SSHOpts {
	opts := guest.SSHOpts{
		User:           cfg.SSHUser,
		KeyPath:        cfg.SSHPrivateKey,
		KeyPEM:         decodeSSHKeyB64(cfg.SSHPrivateKeyB64),
		Host:           host,
		KnownHostsPath: cfg.SSHKnownHosts,
	}
	if cfg.SSHHostKey != "" {
		if pk, err := guest.ParseHostKey(cfg.SSHHostKey); err == nil {
			opts.HostKey = pk
		}
	}
	return opts
}
