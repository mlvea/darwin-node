package main

import (
	"context"

	dnconfig "github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/runtime"
	"github.com/mlvea/darwin-node/pkg/runtime/supervised"
)

// runtimeFor returns the in-process runtime, or the slot-supervisor
// adapter when --vm-supervisor=on. The supervisor process does not call
// this; it builds its own runtime in supervisorRuntime.
func runtimeFor(ctx context.Context, cfg dnconfig.Config) (runtime.Runtime, error) {
	sup := dnconfig.SupervisorFrom(ctx)
	if !sup.Enabled() {
		return newRuntime(cfg)
	}
	return supervised.New(supervised.Options{
		RunDir:        sup.RunDir,
		Spawn:         sup.Spawn,
		BinaryVersion: dnconfig.Version,
		VMRuntime:     cfg.Runtime,
		Deadman:       sup.Deadman,
		Slots:         cfg.MaxVMs,
		LogLevel:      cfg.LogLevel,
	}), nil
}
