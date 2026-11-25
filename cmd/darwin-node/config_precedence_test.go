package main

import (
	"context"
	"io"
	"strings"
	"testing"

	dnconfig "github.com/mlvea/darwin-node/pkg/config"

	"k8s.io/apimachinery/pkg/api/resource"
)

// Precedence is defaults, then environment, then an explicit flag.
// ApplyEnv used to run after flag parsing, so the environment won.
func TestExplicitFlagBeatsEnv(t *testing.T) {
	cases := []struct {
		name    string
		env     map[string]string
		args    []string
		check   func(*testing.T, dnconfig.Config)
		wantErr string
	}{
		{
			name: "max-vms flag beats env",
			env:  map[string]string{"DARWIN_NODE_MAX_VMS": "1"},
			args: []string{"--max-vms", "2"},
			check: func(t *testing.T, c dnconfig.Config) {
				if c.MaxVMs != 2 {
					t.Fatalf("max vms %d, want the flag value 2", c.MaxVMs)
				}
			},
		},
		{
			name: "max-vms env applies when the flag is omitted",
			env:  map[string]string{"DARWIN_NODE_MAX_VMS": "1"},
			check: func(t *testing.T, c dnconfig.Config) {
				if c.MaxVMs != 1 {
					t.Fatalf("max vms %d, want the env value 1", c.MaxVMs)
				}
			},
		},
		{
			name: "explicit max-vms beats an env value that would not validate",
			env:  map[string]string{"DARWIN_NODE_MAX_VMS": "9"},
			args: []string{"--max-vms", "1"},
			check: func(t *testing.T, c dnconfig.Config) {
				if c.MaxVMs != 1 {
					t.Fatalf("max vms %d, want the flag value 1", c.MaxVMs)
				}
			},
		},
		{
			name: "nodename flag beats env",
			env:  map[string]string{"DARWIN_NODE_NAME": "from-env"},
			args: []string{"--nodename", "from-flag"},
			check: func(t *testing.T, c dnconfig.Config) {
				if c.NodeName != "from-flag" {
					t.Fatalf("node name %q", c.NodeName)
				}
			},
		},
		{
			name: "listen address flag beats env",
			env:  map[string]string{"DARWIN_NODE_LISTEN_ADDRESS": "10.0.0.8"},
			args: []string{"--listen-address", "192.168.9.9"},
			check: func(t *testing.T, c dnconfig.Config) {
				if c.ListenAddress != "192.168.9.9" {
					t.Fatalf("listen %q", c.ListenAddress)
				}
			},
		},
		{
			name: "serial console flag beats env",
			env:  map[string]string{"DARWIN_NODE_SERIAL_CONSOLE": "true"},
			args: []string{"--serial-console=false"},
			check: func(t *testing.T, c dnconfig.Config) {
				if c.SerialConsole {
					t.Fatal("serial console stayed on from the environment")
				}
			},
		},
		{
			name: "image gc flag beats env",
			env:  map[string]string{"DARWIN_NODE_IMAGE_GC": "false"},
			args: []string{"--image-gc=true"},
			check: func(t *testing.T, c dnconfig.Config) {
				if !c.ImageGC {
					t.Fatal("image gc stayed off from the environment")
				}
			},
		},
		{
			name: "metrics address flag beats env",
			env:  map[string]string{"DARWIN_NODE_METRICS_ADDRESS": "off"},
			args: []string{"--metrics-address", "127.0.0.1:19102"},
			check: func(t *testing.T, c dnconfig.Config) {
				if c.MetricsAddress != "127.0.0.1:19102" {
					t.Fatalf("metrics %q", c.MetricsAddress)
				}
			},
		},
		{
			name: "reserved env vars are read",
			env: map[string]string{
				"DARWIN_NODE_RESERVED_CPU":       "1500m",
				"DARWIN_NODE_RESERVED_MEMORY":    "6Gi",
				"DARWIN_NODE_RESERVED_EPHEMERAL": "8Gi",
			},
			check: func(t *testing.T, c dnconfig.Config) {
				assertQty(t, "cpu", c.ReservedCPU, "1500m")
				assertQty(t, "memory", c.ReservedMemory, "6Gi")
				assertQty(t, "ephemeral", c.ReservedEphemeral, "8Gi")
			},
		},
		{
			name: "reserved flags beat env",
			env: map[string]string{
				"DARWIN_NODE_RESERVED_CPU":       "1500m",
				"DARWIN_NODE_RESERVED_MEMORY":    "6Gi",
				"DARWIN_NODE_RESERVED_EPHEMERAL": "8Gi",
			},
			args: []string{"--reserved-cpu", "500m", "--reserved-memory", "1Gi", "--reserved-ephemeral", "2Gi"},
			check: func(t *testing.T, c dnconfig.Config) {
				assertQty(t, "cpu", c.ReservedCPU, "500m")
				assertQty(t, "memory", c.ReservedMemory, "1Gi")
				assertQty(t, "ephemeral", c.ReservedEphemeral, "2Gi")
			},
		},
		{
			name: "defaults when env and flags are absent",
			env: map[string]string{
				"DARWIN_NODE_MAX_VMS":            "",
				"DARWIN_NODE_NAME":               "",
				"DARWIN_NODE_LISTEN_ADDRESS":     "",
				"DARWIN_NODE_SERIAL_CONSOLE":     "",
				"DARWIN_NODE_IMAGE_GC":           "",
				"DARWIN_NODE_RESERVED_CPU":       "",
				"DARWIN_NODE_RESERVED_MEMORY":    "",
				"DARWIN_NODE_RESERVED_EPHEMERAL": "",
			},
			check: func(t *testing.T, c dnconfig.Config) {
				d := dnconfig.Default()
				if c.MaxVMs != d.MaxVMs || c.SerialConsole || !c.ImageGC || c.MetricsAddress != d.MetricsAddress {
					t.Fatalf("max %d serial %v gc %v metrics %q", c.MaxVMs, c.SerialConsole, c.ImageGC, c.MetricsAddress)
				}
				assertQty(t, "cpu", c.ReservedCPU, d.ReservedCPU.String())
				assertQty(t, "memory", c.ReservedMemory, d.ReservedMemory.String())
				assertQty(t, "ephemeral", c.ReservedEphemeral, d.ReservedEphemeral.String())
			},
		},
		{
			name:    "malformed reserved cpu is an error",
			env:     map[string]string{"DARWIN_NODE_RESERVED_CPU": "nope"},
			wantErr: "DARWIN_NODE_RESERVED_CPU",
		},
		{
			name: "cache writers env applies when the flag is omitted",
			env:  map[string]string{"DARWIN_NODE_CACHE_WRITERS": "ci, build/updater"},
			check: func(t *testing.T, c dnconfig.Config) {
				if strings.Join(c.CacheWriters, ",") != "ci,build/updater" {
					t.Fatalf("cache writers %v", c.CacheWriters)
				}
			},
		},
		{
			name: "cache writers flag beats env",
			env:  map[string]string{"DARWIN_NODE_CACHE_WRITERS": "ci"},
			args: []string{"--cache-writers", "build/updater"},
			check: func(t *testing.T, c dnconfig.Config) {
				if strings.Join(c.CacheWriters, ",") != "build/updater" {
					t.Fatalf("cache writers %v, want the flag value", c.CacheWriters)
				}
			},
		},
		{
			name: "cache writers flag replaces an env value that would not validate",
			env:  map[string]string{"DARWIN_NODE_CACHE_WRITERS": "a/b/c"},
			args: []string{"--cache-writers", "ci"},
			check: func(t *testing.T, c dnconfig.Config) {
				if strings.Join(c.CacheWriters, ",") != "ci" {
					t.Fatalf("cache writers %v, want the flag value", c.CacheWriters)
				}
			},
		},
		{
			name:    "malformed cache writers env is an error",
			env:     map[string]string{"DARWIN_NODE_CACHE_WRITERS": "a/b/c"},
			wantErr: "cache writer",
		},
		{
			name: "cache writers default to empty",
			env:  map[string]string{"DARWIN_NODE_CACHE_WRITERS": ""},
			check: func(t *testing.T, c dnconfig.Config) {
				if len(c.CacheWriters) != 0 {
					t.Fatalf("cache writers %v, want none", c.CacheWriters)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := resolveForTest(t, tc.env, tc.args)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want it to mention %s", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, cfg)
		})
	}
}

func resolveForTest(t *testing.T, env map[string]string, args []string) (dnconfig.Config, error) {
	t.Helper()
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "")
	t.Setenv("VZ_SSH_PASSWORD", "")
	for k, v := range env {
		t.Setenv(k, v)
	}
	cmd, cfg, _, err := buildCommand(func(context.Context, dnconfig.Config, bool, *shutdownGate) error {
		return nil
	})
	if err != nil {
		return dnconfig.Config{}, err
	}
	cmd.SetArgs(args)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err != nil {
		return *cfg, err
	}
	return *cfg, nil
}

func assertQty(t *testing.T, name string, got resource.Quantity, want string) {
	t.Helper()
	if got.Cmp(resource.MustParse(want)) != 0 {
		t.Fatalf("%s %s, want %s", name, got.String(), want)
	}
}
