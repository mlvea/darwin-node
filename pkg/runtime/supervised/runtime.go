// Package supervised implements runtime.Runtime by talking to per-VM
// supervisor processes. The engine keeps using the runtime interface.
package supervised

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"
)

// Options select how the agent reaches slot supervisors.
type Options struct {
	RunDir        string
	Spawn         string
	Binary        string
	BinaryVersion string
	// VMRuntime is the name reported to the engine. Empty means vz, so
	// image resolution still runs. The supervisor process is the one
	// that actually constructs the VM.
	VMRuntime types.RuntimeName
	Deadman   time.Duration
	Slots     int
	LogLevel  string
	// Dial, when set, replaces the Unix dial. Tests use it so Create
	// does not spawn a process. A dial error is returned as-is.
	Dial func(ctx context.Context, slot int) (*supervisor.Client, error)
}

// Runtime is the agent-side adapter. It is not used inside the supervisor.
type Runtime struct {
	Options

	mu        sync.Mutex
	used      []bool
	skew      supervisor.SkewControl
	holds     map[int]*skewHold
	finishing []*skewHold // removed from holds, Release still running
}

// New returns an adapter.
func New(opts Options) *Runtime {
	if err := supervisor.RaiseNOFILE(); err != nil {
		slog.Warn("node-agent nofile limit", "err", err)
	}
	if opts.LogLevel == "" {
		opts.LogLevel = "info"
	}
	if opts.Deadman <= 0 {
		opts.Deadman = 5 * time.Minute
	}
	return &Runtime{Options: opts}
}

// Name is the underlying VM runtime. The engine treats vz as the runtime
// that needs an image-backed disk.
func (r *Runtime) Name() types.RuntimeName {
	if r.VMRuntime == "" {
		return types.RuntimeVZ
	}
	return r.VMRuntime
}

// Create reserves the lowest free slot and connects to its supervisor.
// The VM is not started until Start.
func (r *Runtime) Create(ctx context.Context, spec types.VMSpec) (runtime.Machine, error) {
	slot, ok := r.alloc()
	if !ok {
		return nil, fmt.Errorf("no free supervisor slot")
	}
	c, err := r.connect(ctx, slot)
	if err != nil {
		r.free(slot)
		return nil, err
	}
	return &machine{
		rt:     r,
		id:     spec.ID,
		spec:   spec,
		slot:   slot,
		client: c,
		status: types.VMStatus{State: types.VMPending},
	}, nil
}

func (r *Runtime) alloc() (int, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensure()
	for i, used := range r.used {
		if !used {
			r.used[i] = true
			return i, true
		}
	}
	return 0, false
}

func (r *Runtime) free(slot int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if slot >= 0 && slot < len(r.used) {
		r.used[slot] = false
	}
}

func (r *Runtime) mark(slot int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensure()
	if slot >= 0 && slot < len(r.used) {
		r.used[slot] = true
	}
}

func (r *Runtime) ensure() {
	if r.used != nil {
		return
	}
	n := r.Slots
	if n <= 0 {
		n = types.AppleMaxConcurrentVMs
	}
	r.used = make([]bool, n)
}

func (r *Runtime) slotCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ensure()
	return len(r.used)
}

func (r *Runtime) connect(ctx context.Context, slot int) (*supervisor.Client, error) {
	c, err := r.dialSlot(ctx, slot)
	if err == nil {
		return c, nil
	}
	if r.Dial != nil {
		return nil, err
	}
	if r.Spawn == "launchd" {
		if err := kickstart(slot); err != nil {
			return nil, err
		}
	} else {
		if _, err := r.spawnSlot(slot); err != nil {
			return nil, err
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	last := err
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		c, last = r.dialSlot(ctx, slot)
		if last == nil {
			return c, nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil, fmt.Errorf("supervisor slot %d did not accept a connection: %w", slot, last)
}

func (r *Runtime) dialSlot(ctx context.Context, slot int) (*supervisor.Client, error) {
	if r.Dial != nil {
		return r.Dial(ctx, slot)
	}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	return supervisor.Dial(dctx, supervisor.SocketPath(r.RunDir, slot), r.version(), slot)
}

func (r *Runtime) version() string {
	if r.BinaryVersion != "" {
		return r.BinaryVersion
	}
	return "dev"
}

func (r *Runtime) binary() string {
	if r.Binary != "" {
		return r.Binary
	}
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return os.Args[0]
	}
	return exe
}

func (r *Runtime) supervisorArgs(slot int) []string {
	level := r.LogLevel
	if level == "" {
		level = "info"
	}
	rt := r.VMRuntime
	if rt == "" {
		rt = types.RuntimeVZ
	}
	return []string{
		"--role=supervisor",
		fmt.Sprintf("--slot=%d", slot),
		"--runtime=" + string(rt),
		"--vm-supervisor-run-dir=" + r.RunDir,
		"--vm-supervisor-deadman=" + r.Deadman.String(),
		"--log-level=" + level,
	}
}
