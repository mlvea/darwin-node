package vzcontract

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTraceSchema(t *testing.T) {
	dir := TestdataDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, ent := range ents {
		if !ent.IsDir() && strings.HasSuffix(ent.Name(), ".jsonl") {
			names = append(names, ent.Name())
		}
	}
	if len(names) == 0 {
		t.Fatal("no seed traces")
	}
	ops := map[string]bool{
		"meta": true, "create": true, "start": true, "stop": true,
		"request_stop": true, "pause": true, "resume": true,
		"save": true, "restore": true, "connect": true, "register": true,
		"crash": true,
	}
	kinds := map[string]bool{
		"state_change": true, "delegate_guest_did_stop": true, "completion_handler": true,
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			lines := readLines(t, filepath.Join(dir, name))
			if len(lines) == 0 || lines[0].Op != "meta" {
				t.Fatal("missing meta")
			}
			if lines[0].Source != "derived" {
				t.Fatalf("source %q, seed traces are derived until a Mac Mini records them", lines[0].Source)
			}
			if !strings.Contains(lines[0].Note, "not hardware-recorded") {
				t.Fatalf("note %q", lines[0].Note)
			}
			for i, line := range lines {
				if line.Seq != i+1 {
					t.Fatalf("seq %d", line.Seq)
				}
				if !ops[line.Op] {
					t.Fatalf("op %q", line.Op)
				}
				if line.Events == nil {
					continue
				}
				for _, ev := range *line.Events {
					if !kinds[ev.Kind] {
						t.Fatalf("kind %q", ev.Kind)
					}
					if ev.Kind == "state_change" && ev.State == "" {
						t.Fatal("state_change without state")
					}
				}
			}
		})
	}
}
