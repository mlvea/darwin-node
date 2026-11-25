package vzcontract

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Line is one JSONL trace record. Events is nil when the line does not
// carry an event list; an empty list asserts that the op recorded nothing.
type Line struct {
	Seq       int      `json:"seq"`
	Op        string   `json:"op"`
	PreState  string   `json:"pre_state,omitempty"`
	Order     string   `json:"order,omitempty"`
	HoldAgent bool     `json:"hold_agent,omitempty"`
	Source    string   `json:"source,omitempty"`
	Note      string   `json:"note,omitempty"`
	VM        string   `json:"vm,omitempty"`
	Events    *[]Event `json:"events,omitempty"`
}

// ReplayDir runs every .jsonl file in dir against factory. Each file's
// meta line selects order and HoldAgent. SimVZ must produce every recorded
// ordering and no completion outside that file's error envelope.
func ReplayDir(t *testing.T, factory Factory, dir string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, ent := range ents {
		if ent.IsDir() || !strings.HasSuffix(ent.Name(), ".jsonl") {
			continue
		}
		n++
		name := ent.Name()
		t.Run(name, func(t *testing.T) {
			prepare(t)
			replayFile(t, factory, filepath.Join(dir, name))
		})
	}
	if n == 0 {
		t.Fatalf("no traces in %s", dir)
	}
}

func replayFile(t *testing.T, factory Factory, path string) {
	t.Helper()
	lines := readLines(t, path)
	if len(lines) == 0 || lines[0].Op != "meta" {
		t.Fatalf("%s: first record must be meta", path)
	}
	meta := lines[0]
	if meta.Source != "derived" && meta.Source != "hardware" {
		t.Fatalf("%s: source %q", path, meta.Source)
	}
	allowed := map[string]bool{}
	for _, line := range lines {
		if line.Events == nil {
			continue
		}
		for _, ev := range *line.Events {
			if ev.Kind == "completion_handler" {
				allowed[errKey(ev.Err)] = true
			}
		}
	}
	rt := factory(t, Config{Order: meta.Order, HoldAgent: meta.HoldAgent})
	vms := map[string]VM{}
	dir := t.TempDir()
	saves := map[string]string{}
	for i, line := range lines {
		if line.Seq != i+1 {
			t.Fatalf("seq %d at index %d", line.Seq, i)
		}
		if line.Op == "meta" {
			continue
		}
		name := line.VM
		if name == "" {
			name = "vm"
		}
		vm := vms[name]
		if line.Op == "create" {
			if vm != nil {
				t.Fatalf("duplicate create %s", name)
			}
			vm = mustCreate(t, rt, specNamed(name))
			vms[name] = vm
		}
		if vm == nil {
			t.Fatalf("op %s before create of %s", line.Op, name)
		}
		if line.PreState != "" && vm.State() != line.PreState {
			t.Fatalf("seq %d pre_state %s, have %s", line.Seq, line.PreState, vm.State())
		}
		before := len(vm.Events())
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := replayOp(ctx, vm, line.Op, name, dir, saves)
		cancel()
		if line.Events != nil {
			got := vm.Events()[before:]
			wantEvents(t, got, *line.Events)
			for _, ev := range got {
				if ev.Kind != "completion_handler" {
					continue
				}
				if !allowed[errKey(ev.Err)] {
					t.Fatalf("seq %d error code outside envelope: %s", line.Seq, errKey(ev.Err))
				}
			}
			if completionFailed(*line.Events) {
				if err == nil {
					t.Fatalf("seq %d op %s succeeded, trace records an error", line.Seq, line.Op)
				}
				requireTraceErr(t, err, *line.Events)
			} else if hasCompletion(*line.Events) && err != nil {
				t.Fatalf("seq %d op %s: %v", line.Seq, line.Op, err)
			}
		} else if err != nil {
			t.Fatalf("seq %d op %s: %v", line.Seq, line.Op, err)
		}
	}
}

func replayOp(ctx context.Context, vm VM, op, name, dir string, saves map[string]string) error {
	switch op {
	case "create":
		return nil
	case "start":
		return vm.Start(ctx)
	case "stop":
		return vm.Stop(ctx)
	case "request_stop":
		return vm.RequestStop(ctx)
	case "pause":
		return vm.Pause(ctx)
	case "resume":
		return vm.Resume(ctx)
	case "save":
		path := filepath.Join(dir, name+".json")
		saves[name] = path
		return vm.Save(ctx, path)
	case "restore":
		path := saves[name]
		if path == "" {
			return fmt.Errorf("restore of %s before save", name)
		}
		return vm.Restore(ctx, path)
	case "connect":
		c, err := vm.Connect(ctx)
		if c != nil {
			_ = c.Close()
		}
		return err
	case "register":
		return vm.RegisterAgent(ctx)
	case "crash":
		return vm.Crash()
	default:
		return fmt.Errorf("unknown op %q", op)
	}
}

func completionFailed(ev []Event) bool {
	for _, e := range ev {
		if e.Kind == "completion_handler" && e.Err != nil {
			return true
		}
	}
	return false
}

func hasCompletion(ev []Event) bool {
	for _, e := range ev {
		if e.Kind == "completion_handler" {
			return true
		}
	}
	return false
}

func requireTraceErr(t *testing.T, err error, ev []Event) {
	t.Helper()
	for _, e := range ev {
		if e.Kind != "completion_handler" || e.Err == nil {
			continue
		}
		if e.Err.Errno != "" {
			n := errnoByName(e.Err.Errno)
			if !errors.Is(err, n) {
				t.Fatalf("error %v, want errno %s", err, e.Err.Errno)
			}
			return
		}
		if e.Err.Code != 0 {
			requireCode(t, err, e.Err.Code, e.Err.Name)
			return
		}
	}
}

func errnoByName(name string) error {
	switch name {
	case "ECONNREFUSED":
		return syscall.ECONNREFUSED
	case "EPIPE":
		return syscall.EPIPE
	case "ECONNRESET":
		return syscall.ECONNRESET
	case "ENOBUFS":
		return syscall.ENOBUFS
	case "EAGAIN":
		return syscall.EAGAIN
	default:
		return fmt.Errorf("unknown errno %s", name)
	}
}

func readLines(t *testing.T, path string) []Line {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var lines []Line
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		var line Line
		if err := json.Unmarshal([]byte(text), &line); err != nil {
			t.Fatalf("%s:%d: %v", path, n, err)
		}
		lines = append(lines, line)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}
