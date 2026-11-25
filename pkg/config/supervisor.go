package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mlvea/darwin-node/pkg/types"
)

const (
	// RoleAgent is the virtual-kubelet process.
	RoleAgent = "agent"
	// RoleSupervisor is one slot's VM owner.
	RoleSupervisor = "supervisor"

	// ModeOff leaves VM ownership inside the agent process.
	ModeOff = "off"
	// ModeOn talks to per-VM supervisors instead.
	ModeOn = "on"

	// SpawnSubprocess is the dev launch path (new session, no launchd).
	SpawnSubprocess = "subprocess"
	// SpawnLaunchd kickstarts the slot's LaunchAgent.
	SpawnLaunchd = "launchd"
)

// Supervisor is the per-VM supervisor feature. It is not a field of Config:
// that struct stays the virtual-kubelet configuration.
type Supervisor struct {
	Role    string
	Mode    string
	Spawn   string
	RunDir  string
	Slot    int
	Deadman time.Duration
}

// DefaultSupervisor is off. Slot -1 means the flag was not set; slot 0 is
// a real slot and must not be the default.
func DefaultSupervisor() Supervisor {
	return Supervisor{
		Role:    RoleAgent,
		Mode:    ModeOff,
		Spawn:   SpawnSubprocess,
		RunDir:  defaultSupervisorRunDir(),
		Slot:    -1,
		Deadman: 5 * time.Minute,
	}
}

func defaultSupervisorRunDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", types.HostLaunchdLabel, "run")
}

// MergeEnv overlays DARWIN_NODE_ROLE, DARWIN_NODE_SLOT, and the
// DARWIN_NODE_VM_SUPERVISOR* variables. An empty variable leaves the
// current value. It does not validate.
func (s *Supervisor) MergeEnv() error {
	if s == nil {
		return fmt.Errorf("nil supervisor config")
	}
	if v := os.Getenv("DARWIN_NODE_ROLE"); v != "" {
		s.Role = v
	}
	if v := os.Getenv("DARWIN_NODE_SLOT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("DARWIN_NODE_SLOT: %w", err)
		}
		s.Slot = n
	}
	if v := os.Getenv("DARWIN_NODE_VM_SUPERVISOR"); v != "" {
		s.Mode = v
	}
	if v := os.Getenv("DARWIN_NODE_VM_SUPERVISOR_RUN_DIR"); v != "" {
		s.RunDir = v
	}
	if v := os.Getenv("DARWIN_NODE_VM_SUPERVISOR_DEADMAN"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("DARWIN_NODE_VM_SUPERVISOR_DEADMAN: %w", err)
		}
		s.Deadman = d
	}
	if v := os.Getenv("DARWIN_NODE_VM_SUPERVISOR_SPAWN"); v != "" {
		s.Spawn = v
	}
	return nil
}

// Normalize lowercases the string fields and fills empties with defaults.
func (s *Supervisor) Normalize() {
	if s == nil {
		return
	}
	s.Role = strings.ToLower(strings.TrimSpace(s.Role))
	s.Mode = strings.ToLower(strings.TrimSpace(s.Mode))
	s.Spawn = strings.ToLower(strings.TrimSpace(s.Spawn))
	if s.Role == "" {
		s.Role = RoleAgent
	}
	if s.Mode == "" {
		s.Mode = ModeOff
	}
	if s.Spawn == "" {
		s.Spawn = SpawnSubprocess
	}
}

// Validate checks the role, the feature flag, and the slot.
func (s *Supervisor) Validate() error {
	if s == nil {
		return fmt.Errorf("nil supervisor config")
	}
	s.Normalize()
	switch s.Role {
	case RoleAgent, RoleSupervisor:
	default:
		return fmt.Errorf("role must be agent or supervisor, got %q", s.Role)
	}
	switch s.Mode {
	case ModeOff, ModeOn:
	default:
		return fmt.Errorf("vm supervisor must be off or on, got %q", s.Mode)
	}
	switch s.Spawn {
	case SpawnSubprocess, SpawnLaunchd:
	default:
		return fmt.Errorf("vm supervisor spawn must be subprocess or launchd, got %q", s.Spawn)
	}
	if s.Deadman <= 0 {
		return fmt.Errorf("vm supervisor deadman must be positive, got %s", s.Deadman)
	}
	if s.Slot < -1 || s.Slot >= types.AppleMaxConcurrentVMs {
		return fmt.Errorf("slot must be 0 or 1, got %d", s.Slot)
	}
	if s.Role == RoleSupervisor && s.Slot < 0 {
		return fmt.Errorf("supervisor slot must be 0 or 1, got %d", s.Slot)
	}
	if s.Role == RoleSupervisor && s.RunDir == "" {
		return fmt.Errorf("supervisor run dir is empty")
	}
	if s.Enabled() && s.RunDir == "" {
		return fmt.Errorf("vm supervisor run dir is empty")
	}
	return nil
}

// Enabled reports whether the agent should own VMs through supervisors.
func (s Supervisor) Enabled() bool {
	return strings.EqualFold(strings.TrimSpace(s.Mode), ModeOn)
}

type supervisorCtxKey struct{}

// WithSupervisor returns a context carrying sup.
func WithSupervisor(ctx context.Context, sup Supervisor) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, supervisorCtxKey{}, sup)
}

// SupervisorFrom returns the supervisor config on ctx, or the defaults
// when the context does not carry one.
func SupervisorFrom(ctx context.Context) Supervisor {
	if ctx == nil {
		return DefaultSupervisor()
	}
	sup, ok := ctx.Value(supervisorCtxKey{}).(Supervisor)
	if !ok {
		return DefaultSupervisor()
	}
	return sup
}
