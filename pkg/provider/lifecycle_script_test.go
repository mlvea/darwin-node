package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// hardwareReplayPath is the fault-free half of the FuzzNodeLifecycle seed
// corpus, decoded into operations a later Mac Mini gate can replay.
// Scripts that inject a guest crash or a start failure stay on SimVZ.
const hardwareReplayPath = "testdata/fuzz/node_lifecycle_hardware.jsonl"

const (
	lifeCreate  = "create"
	lifeDelete  = "delete"
	lifeCrash   = "crash"
	lifeFail    = "failstart"
	lifeRestart = "restart"
	lifeDrain   = "drain"

	// maxLifeOps bounds one fuzz input. A longer byte string is cut so a
	// single case cannot run past the per-step deadlock watchdog budget.
	maxLifeOps = 32
)

// lifeOp is one step. Slot selects vm-0..vm-3. Policy is set on create.
// Failures is how many upcoming Start calls fail.
type lifeOp struct {
	Op       string `json:"op"`
	Slot     int    `json:"slot,omitempty"`
	Policy   string `json:"policy,omitempty"`
	Failures int    `json:"failures,omitempty"`
}

type lifeSeed struct {
	Name string
	Data []byte
}

type hardwareReplay struct {
	Seed string   `json:"seed"`
	Ops  []lifeOp `json:"ops"`
}

// lifeSeeds is the committed corpus. testdata/fuzz/FuzzNodeLifecycle holds
// the same bytes in the Go fuzz file format; TestSeedCorpusMatchesSource
// keeps the two copies together.
func lifeSeeds() []lifeSeed {
	return []lifeSeed{
		{Name: "empty", Data: []byte{}},
		{Name: "create-delete-drain", Data: []byte{0, 0, 1, 0, 5, 0}},
		{Name: "two-vms-third-drain", Data: []byte{0, 0, 0, 1, 0, 2, 5, 0}},
		{Name: "create-crash-drain", Data: []byte{0, 0, 2, 0, 5, 0}},
		{Name: "failstart-create-drain", Data: []byte{3, 0, 0, 0, 5, 0}},
		{Name: "create-restart-drain", Data: []byte{0, 0, 4, 0, 5, 0}},
		{Name: "create-two-delete-one", Data: []byte{0, 0, 0, 1, 1, 0, 5, 0}},
		{Name: "drain-then-create", Data: []byte{0, 0, 5, 0, 0, 1}},
		{Name: "delete-absent", Data: []byte{1, 0, 5, 0}},
		{Name: "restart-idle", Data: []byte{4, 0, 5, 0}},
		{Name: "never-restart-drain", Data: []byte{0, 8, 4, 0, 5, 0}},
		{Name: "onfailure-delete", Data: []byte{0, 4, 1, 0, 5, 0}},
		{Name: "onfailure-restart-drain", Data: []byte{0, 4, 4, 0, 5, 0}},
		{Name: "refill-slot", Data: []byte{0, 0, 1, 0, 0, 0, 5, 0}},
		{Name: "restart-after-delete", Data: []byte{0, 0, 1, 0, 4, 0, 5, 0}},
		{Name: "never-and-always", Data: []byte{0, 8, 0, 1, 4, 0, 5, 0}},
		{Name: "crash-then-delete", Data: []byte{0, 0, 2, 0, 1, 0, 5, 0}},
		{Name: "double-failstart", Data: []byte{3, 1, 0, 0, 5, 0}},
		{Name: "crash-one-of-two", Data: []byte{0, 0, 0, 1, 2, 1, 5, 0}},
		{Name: "never-crash-restart", Data: []byte{0, 8, 2, 0, 4, 0, 5, 0}},
	}
}

// decodeLife reads a fuzz input. Each operation is two bytes: an opcode
// (mod 6) and an argument. The argument's low bits pick the pod slot.
// Create stores the restart policy in the next two bits. Failstart stores
// a failure count of 1..4. A trailing odd byte is ignored.
func decodeLife(data []byte) []lifeOp {
	if len(data)%2 == 1 {
		data = data[:len(data)-1]
	}
	if len(data) > maxLifeOps*2 {
		data = data[:maxLifeOps*2]
	}
	ops := make([]lifeOp, 0, len(data)/2)
	for i := 0; i < len(data); i += 2 {
		op := int(data[i] % 6)
		arg := int(data[i+1])
		slot := arg & 3
		switch op {
		case 0:
			ops = append(ops, lifeOp{Op: lifeCreate, Slot: slot, Policy: string(lifePolicy(arg))})
		case 1:
			ops = append(ops, lifeOp{Op: lifeDelete, Slot: slot})
		case 2:
			ops = append(ops, lifeOp{Op: lifeCrash, Slot: slot})
		case 3:
			ops = append(ops, lifeOp{Op: lifeFail, Failures: (arg & 3) + 1})
		case 4:
			ops = append(ops, lifeOp{Op: lifeRestart})
		default:
			ops = append(ops, lifeOp{Op: lifeDrain})
		}
	}
	return ops
}

func lifePolicy(arg int) corev1.RestartPolicy {
	switch (arg >> 2) & 3 {
	case 1:
		return corev1.RestartPolicyOnFailure
	case 2:
		return corev1.RestartPolicyNever
	default:
		return corev1.RestartPolicyAlways
	}
}

func faultFree(ops []lifeOp) bool {
	for _, op := range ops {
		if op.Op == lifeCrash || op.Op == lifeFail {
			return false
		}
	}
	return true
}

func scriptJSON(ops []lifeOp) string {
	if ops == nil {
		ops = []lifeOp{}
	}
	b, err := json.Marshal(ops)
	if err != nil {
		return fmt.Sprintf("%v", ops)
	}
	return string(b)
}

func faultFreeReplay(seeds []lifeSeed) []hardwareReplay {
	out := make([]hardwareReplay, 0, len(seeds))
	for _, s := range seeds {
		ops := decodeLife(s.Data)
		if !faultFree(ops) {
			continue
		}
		if ops == nil {
			ops = []lifeOp{}
		}
		out = append(out, hardwareReplay{Seed: s.Name, Ops: ops})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seed < out[j].Seed })
	return out
}

func parseCorpus(b []byte) ([]byte, error) {
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "go test fuzz v1" {
		return nil, fmt.Errorf("bad fuzz corpus header")
	}
	var data []byte
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "[]byte(") || !strings.HasSuffix(line, ")") {
			return nil, fmt.Errorf("bad corpus value %q", line)
		}
		quoted := strings.TrimSuffix(strings.TrimPrefix(line, "[]byte("), ")")
		s, err := strconv.Unquote(quoted)
		if err != nil {
			return nil, err
		}
		data = append(data, s...)
	}
	if data == nil {
		data = []byte{}
	}
	return data, nil
}

func TestSeedCorpusMatchesSource(t *testing.T) {
	dir := filepath.Join("testdata", "fuzz", "FuzzNodeLifecycle")
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	for _, ent := range ents {
		if ent.IsDir() {
			t.Fatalf("corpus directory %s", ent.Name())
		}
		b, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		data, err := parseCorpus(b)
		if err != nil {
			t.Fatalf("%s: %v", ent.Name(), err)
		}
		got[ent.Name()] = data
	}
	want := lifeSeeds()
	if len(got) != len(want) {
		t.Fatalf("corpus has %d files, source lists %d", len(got), len(want))
	}
	for _, s := range want {
		data, ok := got[s.Name]
		if !ok {
			t.Fatalf("missing corpus file %s", s.Name)
		}
		if !bytes.Equal(data, s.Data) {
			t.Fatalf("%s bytes %x, source %x", s.Name, data, s.Data)
		}
	}
}

func TestHardwareReplayListMatchesFaultFreeSeeds(t *testing.T) {
	want := faultFreeReplay(lifeSeeds())
	b, err := os.ReadFile(hardwareReplayPath)
	if err != nil {
		t.Fatal(err)
	}
	text := strings.TrimSpace(strings.ReplaceAll(string(b), "\r\n", "\n"))
	var got []hardwareReplay
	if text != "" {
		for _, line := range strings.Split(text, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var row hardwareReplay
			if err := json.Unmarshal([]byte(line), &row); err != nil {
				t.Fatalf("replay line: %v\n%s", err, line)
			}
			if row.Ops == nil {
				row.Ops = []lifeOp{}
			}
			got = append(got, row)
		}
	}
	sort.Slice(got, func(i, j int) bool { return got[i].Seed < got[j].Seed })
	if !reflect.DeepEqual(got, want) {
		wb, _ := json.MarshalIndent(want, "", "  ")
		gb, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("hardware replay list != fault-free seeds\nwant %s\ngot  %s", wb, gb)
	}
	for _, row := range got {
		if !faultFree(row.Ops) {
			t.Fatalf("seed %s injects a fault: %s", row.Seed, scriptJSON(row.Ops))
		}
	}
}
