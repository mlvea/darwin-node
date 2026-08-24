package debug

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/node"
	"github.com/darwin-node/darwin-node/pkg/types"

	corev1 "k8s.io/api/core/v1"
)

//go:embed assets/debug.html assets/debug.js
var pageFS embed.FS

// Snapshot is the JSON the HTML debugger consumes.
type Snapshot struct {
	GeneratedAt string            `json:"generatedAt"`
	Node        NodeSnap          `json:"node"`
	Slots       SlotsSnap         `json:"slots"`
	Pods        []engine.DebugPod `json:"pods"`
	Agents      []AgentSnap       `json:"agents"`
}

type NodeSnap struct {
	Name           string            `json:"name"`
	Runtime        string            `json:"runtime"`
	KubeletVersion string            `json:"kubeletVersion"`
	Capacity       map[string]string `json:"capacity"`
	Conditions     []CondSnap        `json:"conditions"`
}

type CondSnap struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type SlotsSnap struct {
	Used int      `json:"used"`
	Max  int      `json:"max"`
	Free int      `json:"free"`
	UIDs []string `json:"uids"`
}

type AgentSnap struct {
	Pod     string `json:"pod"`
	OK      bool   `json:"ok"`
	VMState string `json:"vmState"`
}

func Capture(cfg config.Config, inv node.Inventory, eng *engine.Engine) Snapshot {
	n := &corev1.Node{}
	inv.Cfg = cfg
	if eng != nil {
		inv.Slots = eng.Slots()
	}
	_ = node.Apply(context.Background(), n, inv)
	cap := map[string]string{}
	for k, v := range n.Status.Capacity {
		cap[string(k)] = v.String()
	}
	var conds []CondSnap
	for _, c := range n.Status.Conditions {
		conds = append(conds, CondSnap{Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message})
	}
	used, max, pods := 0, types.AppleMaxConcurrentVMs, []engine.DebugPod{}
	var uids []string
	if eng != nil {
		used, max, pods = eng.DebugSnapshot()
		uids = eng.Slots().UIDs()
	}
	var agents []AgentSnap
	for _, p := range pods {
		agents = append(agents, AgentSnap{Pod: p.Namespace + "/" + p.Name, OK: p.AgentOK, VMState: p.VMState})
	}
	return Snapshot{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Node: NodeSnap{
			Name:           cfg.NodeName,
			Runtime:        string(cfg.Runtime),
			KubeletVersion: n.Status.NodeInfo.KubeletVersion,
			Capacity:       cap,
			Conditions:     conds,
		},
		Slots: SlotsSnap{
			Used: used,
			Max:  max,
			Free: max - used,
			UIDs: uids,
		},
		Pods:   pods,
		Agents: agents,
	}
}

func (s Snapshot) Write(path string) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return err
	}
	jsPath := filepath.Join(dir, "snapshot.js")
	if dir == "" || dir == "." {
		jsPath = "snapshot.js"
	}
	js := "window.DARWIN_NODE_SNAPSHOT = " + string(raw) + ";\n"
	if err := os.WriteFile(jsPath, []byte(js), 0o644); err != nil {
		return err
	}
	return writePageAssets(dir)
}

func writePageAssets(dir string) error {
	if dir == "" {
		dir = "."
	}
	return fs.WalkDir(pageFS, "assets", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := pageFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, filepath.Base(path)), b, 0o644)
	})
}
