package config

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestSupervisorDefaults(t *testing.T) {
	s := DefaultSupervisor()
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Enabled() || s.Role != RoleAgent || s.Mode != ModeOff || s.Slot != -1 {
		t.Fatalf("defaults %+v", s)
	}
	if s.Deadman != 5*time.Minute || s.Spawn != SpawnSubprocess {
		t.Fatalf("defaults %+v", s)
	}
	if SupervisorFrom(context.Background()).Role != RoleAgent {
		t.Fatal("missing context")
	}
}

func TestSupervisorValidate(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Supervisor)
		want string
	}{
		{"role", func(s *Supervisor) { s.Role = "both" }, "role"},
		{"mode", func(s *Supervisor) { s.Mode = "maybe" }, "off or on"},
		{"spawn", func(s *Supervisor) { s.Spawn = "systemd" }, "spawn"},
		{"deadman", func(s *Supervisor) { s.Deadman = 0 }, "deadman"},
		{"slot", func(s *Supervisor) { s.Slot = 3 }, "slot"},
		{"supervisor slot", func(s *Supervisor) { s.Role = RoleSupervisor; s.Slot = -1 }, "slot"},
		{"supervisor dir", func(s *Supervisor) { s.Role = RoleSupervisor; s.Slot = 0; s.RunDir = "" }, "run dir"},
		{"mode dir", func(s *Supervisor) { s.Mode = ModeOn; s.RunDir = "" }, "run dir"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := DefaultSupervisor()
			tc.edit(&s)
			err := s.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSupervisorEnv(t *testing.T) {
	t.Setenv("DARWIN_NODE_ROLE", "Supervisor")
	t.Setenv("DARWIN_NODE_SLOT", "1")
	t.Setenv("DARWIN_NODE_VM_SUPERVISOR", "ON")
	t.Setenv("DARWIN_NODE_VM_SUPERVISOR_RUN_DIR", "/tmp/run")
	t.Setenv("DARWIN_NODE_VM_SUPERVISOR_DEADMAN", "30s")
	t.Setenv("DARWIN_NODE_VM_SUPERVISOR_SPAWN", "launchd")
	s := DefaultSupervisor()
	if err := s.MergeEnv(); err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(); err != nil {
		t.Fatal(err)
	}
	if s.Role != RoleSupervisor || s.Slot != 1 || !s.Enabled() || s.RunDir != "/tmp/run" {
		t.Fatalf("%+v", s)
	}
	if s.Deadman != 30*time.Second || s.Spawn != SpawnLaunchd {
		t.Fatalf("%+v", s)
	}
	ctx := WithSupervisor(context.Background(), s)
	if !SupervisorFrom(ctx).Enabled() {
		t.Fatal("context lost the flag")
	}
}

func TestSupervisorEnvRejectsBadNumbers(t *testing.T) {
	t.Setenv("DARWIN_NODE_SLOT", "nope")
	s := DefaultSupervisor()
	if err := s.MergeEnv(); err == nil {
		t.Fatal("expected slot error")
	}
	t.Setenv("DARWIN_NODE_SLOT", "")
	t.Setenv("DARWIN_NODE_VM_SUPERVISOR_DEADMAN", "later")
	s = DefaultSupervisor()
	if err := s.MergeEnv(); err == nil {
		t.Fatal("expected deadman error")
	}
}
