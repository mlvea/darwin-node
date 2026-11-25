package main

import (
	"fmt"
	"time"

	"github.com/mlvea/darwin-node/pkg/capacity"
	dnconfig "github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/debug"
	"github.com/mlvea/darwin-node/pkg/engine"
	"github.com/mlvea/darwin-node/pkg/event"
	"github.com/mlvea/darwin-node/pkg/node"
	"github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/sidecar"
	"github.com/mlvea/darwin-node/pkg/types"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

func cmdDebugDump(cfg *dnconfig.Config) *cobra.Command {
	var out string
	c := &cobra.Command{
		Use:   "debug-dump",
		Short: "Write a JSON snapshot of node/slot/pod state for web/debug.html",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := cfg.Validate(); err != nil {
				return err
			}
			cfg.Runtime = types.RuntimeFake
			cfg.AllowNATWorkloads = true
			if cfg.NodeName == "" {
				cfg.NodeName = "debug-mac"
			}
			slots, err := capacity.New(cfg.MaxVMs)
			if err != nil {
				return err
			}
			eng := engine.New(*cfg, slots, fake.New(), sidecar.NewMemory(), event.Nop{}, "127.0.0.1")
			inv := node.Inventory{
				Host:  node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30, Arch: "arm64", InternalIP: "127.0.0.1"},
				Cfg:   *cfg,
				Slots: slots,
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "sample", Namespace: "default", UID: k8stypes.UID("debug-sample")},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "macos", Image: "local/macos:test"}}},
			}
			if err := eng.Create(cmd.Context(), pod, engine.Credentials{}); err != nil {
				return err
			}
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				if p, err := eng.Get("default", "sample"); err == nil && p.Status.Phase == corev1.PodRunning {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			snap := debug.Capture(*cfg, inv, eng)
			if out == "" {
				out = "debug-snapshot.json"
			}
			if err := snap.Write(out); err != nil {
				return err
			}
			fmt.Println(out)
			return nil
		},
	}
	c.Flags().StringVarP(&out, "out", "o", "debug-snapshot.json", "JSON snapshot path (also writes snapshot.js beside it)")
	c.Flags().StringVar(&cfg.NodeName, "nodename", cfg.NodeName, "node name recorded in the snapshot")
	return c
}
