package main

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	dnconfig "github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/supervisor"
)

func TestVMSupervisorDefaultsOff(t *testing.T) {
	clearSupervisorEnv(t)
	var got dnconfig.Supervisor
	cmd, _, _, err := buildCommand(func(ctx context.Context, _ dnconfig.Config, _ bool, _ *shutdownGate) error {
		got = dnconfig.SupervisorFrom(ctx)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.Enabled() || got.Role != dnconfig.RoleAgent {
		t.Fatalf("supervisor config %+v", got)
	}
}

func TestVMSupervisorFlagBeatsEnv(t *testing.T) {
	clearSupervisorEnv(t)
	t.Setenv("DARWIN_NODE_VM_SUPERVISOR", "on")
	t.Setenv("DARWIN_NODE_VM_SUPERVISOR_RUN_DIR", t.TempDir())
	var got dnconfig.Supervisor
	cmd, _, _, err := buildCommand(func(ctx context.Context, _ dnconfig.Config, _ bool, _ *shutdownGate) error {
		got = dnconfig.SupervisorFrom(ctx)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd.SetArgs([]string{"--vm-supervisor=off"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if got.Enabled() {
		t.Fatal("explicit --vm-supervisor=off lost to the environment")
	}
}

func TestSupervisorRoleSkipsAgent(t *testing.T) {
	clearSupervisorEnv(t)
	// macOS sun_path holds 103 bytes. t.TempDir under /var/folders does not.
	dir, err := os.MkdirTemp("/tmp", "dn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	started := make(chan struct{}, 1)
	cmd, _, _, err := buildCommand(func(context.Context, dnconfig.Config, bool, *shutdownGate) error {
		started <- struct{}{}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cmd.SetArgs([]string{
		"--role=supervisor",
		"--slot=0",
		"--runtime=fake",
		"--vm-supervisor-run-dir=" + dir,
		"--vm-supervisor-deadman=1h",
	})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- cmd.ExecuteContext(ctx)
	}()
	path := supervisor.SocketPath(dir, 0)
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(path); err == nil {
			break
		}
		select {
		case err := <-errc:
			t.Fatalf("supervisor exited early: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("supervisor socket did not appear")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-started:
		t.Fatal("agent starter ran for --role=supervisor")
	default:
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not exit after cancel")
	}
}

func clearSupervisorEnv(t *testing.T) {
	t.Helper()
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "")
	t.Setenv("VZ_SSH_PASSWORD", "")
	t.Setenv("DARWIN_NODE_ROLE", "")
	t.Setenv("DARWIN_NODE_SLOT", "")
	t.Setenv("DARWIN_NODE_VM_SUPERVISOR", "")
	t.Setenv("DARWIN_NODE_VM_SUPERVISOR_RUN_DIR", "")
	t.Setenv("DARWIN_NODE_VM_SUPERVISOR_DEADMAN", "")
	t.Setenv("DARWIN_NODE_VM_SUPERVISOR_SPAWN", "")
}
