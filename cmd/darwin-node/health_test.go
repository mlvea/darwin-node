package main

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/node"
	"github.com/mlvea/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

func TestConfigureNodeHealthMarksVZUnavailable(t *testing.T) {
	if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
		t.Skip("vz availability is a hardware fact on Apple Silicon")
	}
	cfg := config.Default()
	cfg.Runtime = types.RuntimeVZ
	cfg.CacheDir = t.TempDir()
	host := node.Host{LogicalCPUs: 4, MemoryBytes: 8 << 30, DiskBytes: 10 << 30}
	np := node.NewStatusProvider(node.Inventory{Host: host, Cfg: cfg})
	np.Probe = func(context.Context) (node.Host, error) { return host, nil }
	configureNodeHealth(np, cfg)
	if err := np.Ping(context.Background()); err != nil {
		t.Fatalf("Ping returned %v; virtual-kubelet would freeze node status", err)
	}
	inv := np.Inventory()
	var ready, guest corev1.NodeCondition
	for _, c := range node.Conditions(inv) {
		switch c.Type {
		case corev1.NodeReady:
			ready = c
		case "GuestRuntime":
			guest = c
		}
	}
	if ready.Status != corev1.ConditionFalse {
		t.Fatalf("NodeReady %s, vz unavailable left the node Ready", ready.Status)
	}
	if guest.Status != corev1.ConditionFalse || !strings.Contains(guest.Message, "darwin/arm64") {
		t.Fatalf("GuestRuntime %+v, want unavailable on this host", guest)
	}
	if err := np.NotReady(); err == nil {
		t.Fatal("vz unavailable still admitted pods")
	}
}

func TestConfigureNodeHealthLeavesFakeRuntimeReady(t *testing.T) {
	cfg := config.Default()
	cfg.Runtime = types.RuntimeFake
	cfg.CacheDir = t.TempDir()
	host := node.Host{LogicalCPUs: 4, MemoryBytes: 8 << 30, DiskBytes: 10 << 30, DiskUsedFrac: 0.1}
	np := node.NewStatusProvider(node.Inventory{Host: host, Cfg: cfg})
	np.Probe = func(context.Context) (node.Host, error) { return host, nil }
	configureNodeHealth(np, cfg)
	if err := np.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, c := range node.Conditions(np.Inventory()) {
		if c.Type == corev1.NodeReady && c.Status != corev1.ConditionTrue {
			t.Fatalf("fake runtime NodeReady %s (%s)", c.Status, c.Message)
		}
		if c.Type == "GuestRuntime" && c.Status != corev1.ConditionTrue {
			t.Fatalf("fake runtime GuestRuntime %s", c.Status)
		}
	}
	if err := np.NotReady(); err != nil {
		t.Fatal(err)
	}
}
