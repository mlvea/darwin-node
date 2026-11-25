package main

import (
	"context"

	dnconfig "github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	vzrt "github.com/mlvea/darwin-node/pkg/runtime/vz"
	"github.com/mlvea/darwin-node/pkg/supervisor"
	"github.com/mlvea/darwin-node/pkg/types"

	"github.com/spf13/cobra"
)

// registerSupervisorFlags adds --role, --slot, and the vm-supervisor flag
// group. buildCommand calls it. The values are not fields of Config.
func registerSupervisorFlags(cmd *cobra.Command, sup *dnconfig.Supervisor) {
	f := cmd.Flags()
	f.StringVar(&sup.Role, "role", sup.Role, "agent (default) or supervisor")
	f.IntVar(&sup.Slot, "slot", sup.Slot, "supervisor slot, 0 or 1")
	f.StringVar(&sup.Mode, "vm-supervisor", sup.Mode, "off (default) or on; on moves each VM into a slot supervisor")
	f.StringVar(&sup.RunDir, "vm-supervisor-run-dir", sup.RunDir, "directory for slot sockets and boot leases (mode 0700)")
	f.DurationVar(&sup.Deadman, "vm-supervisor-deadman", sup.Deadman, "stop the VM when no agent has been connected for this long")
	f.StringVar(&sup.Spawn, "vm-supervisor-spawn", sup.Spawn, "subprocess (dev) or launchd")
}

// runSupervisor is `darwin-node --role=supervisor`. It serves one slot and
// returns nil after the VM stops so launchd does not restart a clean exit.
func runSupervisor(ctx context.Context, cfg dnconfig.Config, sup dnconfig.Supervisor) error {
	rt, err := supervisorRuntime(cfg)
	if err != nil {
		return err
	}
	return (&supervisor.Server{
		Slot:          sup.Slot,
		RunDir:        sup.RunDir,
		RT:            rt,
		BinaryVersion: dnconfig.Version,
		Deadman:       sup.Deadman,
	}).Serve(ctx)
}

// supervisorRuntime is the VM backend inside the supervisor process.
// It is not wrapped in the metrics meter: that wrapper hides the optional
// pause and channel methods this process has to serve.
func supervisorRuntime(cfg dnconfig.Config) (runtime.Runtime, error) {
	if cfg.Runtime == types.RuntimeFake {
		rt := fake.New()
		rt.SetBridgeGuest(true)
		return rt, nil
	}
	return vzrt.New(runtime.Options{
		CacheDir:          cfg.CacheDir,
		NetworkMode:       cfg.NetworkMode,
		BridgeDevice:      cfg.BridgeInterface,
		Graphics:          cfg.Graphics,
		HostAgentVer:      dnconfig.Version,
		SerialConsole:     cfg.SerialConsole,
		FileHandleNetwork: true,
	}), nil
}
