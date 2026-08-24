package debug

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/darwin-node/darwin-node/pkg/capacity"
	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/event"
	"github.com/darwin-node/darwin-node/pkg/node"
	"github.com/darwin-node/darwin-node/pkg/runtime/fake"
	"github.com/darwin-node/darwin-node/pkg/sidecar"
	"github.com/darwin-node/darwin-node/pkg/types"
)

func TestCaptureAndWrite(t *testing.T) {
	slots, _ := capacity.New(2)
	cfg := config.Default()
	cfg.NodeName = "debug-mac"
	cfg.Runtime = types.RuntimeFake
	cfg.AllowNATWorkloads = true
	eng := engine.New(cfg, slots, fake.New(), sidecar.None{}, event.Nop{}, "10.0.0.1")
	inv := node.Inventory{
		Host:  node.Host{LogicalCPUs: 8, MemoryBytes: 16 << 30, DiskBytes: 100 << 30, Arch: "arm64"},
		Cfg:   cfg,
		Slots: slots,
	}
	snap := Capture(cfg, inv, eng)
	if snap.Slots.Max != 2 || snap.Node.Name != "debug-mac" {
		t.Fatalf("%+v", snap)
	}
	if snap.Node.Capacity["pods"] == "" && snap.Node.Capacity["cpu"] == "" {
		t.Fatalf("empty capacity %+v", snap.Node.Capacity)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "debug-snapshot.json")
	if err := snap.Write(path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"slots"`) || !strings.Contains(string(b), `"node"`) {
		t.Fatalf("json %s", b)
	}
	js, err := os.ReadFile(filepath.Join(dir, "snapshot.js"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "window.DARWIN_NODE_SNAPSHOT") {
		t.Fatalf("js %s", js)
	}
	html, err := os.ReadFile(filepath.Join(dir, "debug.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), `<script src="debug.js">`) || strings.Contains(string(html), `type="module"`) {
		t.Fatalf("debug.html must be a file:// page with plain script tags")
	}
	pageJS, err := os.ReadFile(filepath.Join(dir, "debug.js"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pageJS), "require(") || strings.Contains(string(pageJS), "export ") {
		t.Fatal("debug.js must run as a window-global script")
	}
}

func TestDebugJSIsNotAModule(t *testing.T) {
	for _, root := range []string{filepath.Join("assets"), filepath.Join("..", "..", "web")} {
		html, err := os.ReadFile(filepath.Join(root, "debug.html"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(html), `type="module"`) {
			t.Fatal("file:// pages must not use ES modules")
		}
		js, err := os.ReadFile(filepath.Join(root, "debug.js"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(js), "require(") || strings.Contains(string(js), "export ") {
			t.Fatal("debug.js must run as a window-global script")
		}
	}
}
