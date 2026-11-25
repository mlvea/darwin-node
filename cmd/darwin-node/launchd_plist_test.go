package main

import (
	"encoding/xml"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/config"
)

func TestLaunchdPlistPinsClientCAAndListenAddress(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "launchd", "io.darwin-node.node.plist")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	args, env, _, _, err := parseLaunchdPlist(b)
	if err != nil {
		t.Fatal(err)
	}
	ca := strings.TrimSpace(env["APISERVER_CA_CERT_LOCATION"])
	if ca == "" {
		t.Fatal("launchd plist does not set APISERVER_CA_CERT_LOCATION; cluster mode would serve anonymous exec/logs")
	}
	listen := flagValue(args, "--listen-address")
	switch listen {
	case "":
		t.Fatal("launchd plist does not pass --listen-address; the kubelet port follows the process default")
	case "0.0.0.0", "::", "[::]":
		t.Fatalf("launchd plist binds the kubelet on every interface (%s)", listen)
	}
}

// launchd sends SIGKILL when ExitTimeOut elapses after SIGTERM. The
// drain budget is --shutdown-grace-period (default 60s). Close then
// waits up to 30s, and process exit waits up to 5s more, before the
// cache snapshot is finished. ExitTimeOut has to cover that whole
// window or launchd kills the process and the snapshot does not run.
func TestLaunchdPlistExitTimeOutExceedsShutdownGrace(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "launchd", "io.darwin-node.node.plist")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	_, _, ints, _, err := parseLaunchdPlist(b)
	if err != nil {
		t.Fatal(err)
	}
	const closeAndReap = 35 * time.Second // defaultCloseTimeout 30s + defaultReapTimeout 5s
	grace := config.Default().ShutdownGrace
	need := grace + closeAndReap
	got := time.Duration(ints["ExitTimeOut"]) * time.Second
	if got <= grace {
		t.Fatalf("ExitTimeOut %s is not above --shutdown-grace-period %s; launchd SIGKILL arrives before the drain finishes and the cache snapshot does not run", got, grace)
	}
	if got <= need {
		t.Fatalf("ExitTimeOut %s must exceed shutdown grace %s plus %s for Close and the final reap, or the cache snapshot is killed", got, grace, closeAndReap)
	}
}

// launchd writes stdout and stderr to two files and never reopens them.
// newsyslog is what bounds those files. The rule file is installed at
// /etc/newsyslog.d/darwin-node.conf.
func TestNewsyslogRotatesLaunchdLogs(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	root := filepath.Join(filepath.Dir(file), "..", "..")
	plist, err := os.ReadFile(filepath.Join(root, "deploy", "launchd", "io.darwin-node.node.plist"))
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, top, err := parseLaunchdPlist(plist)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{top["StandardOutPath"], top["StandardErrorPath"]}
	if want[0] == "" || want[1] == "" || want[0] == want[1] {
		t.Fatalf("plist log paths = %q", want)
	}
	conf, err := os.ReadFile(filepath.Join(root, "deploy", "launchd", "darwin-node.conf"))
	if err != nil {
		t.Fatal(err)
	}
	rules, err := parseNewsyslog(conf)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]newsyslogRule{}
	for _, r := range rules {
		if _, dup := got[r.path]; dup {
			t.Fatalf("duplicate newsyslog path %s", r.path)
		}
		got[r.path] = r
	}
	if len(got) != len(want) {
		t.Fatalf("newsyslog rules = %d, plist log files = %d", len(got), len(want))
	}
	for _, path := range want {
		r, ok := got[path]
		if !ok {
			t.Fatalf("no newsyslog rule for %s", path)
		}
		if r.owner != "root" || r.group != "admin" {
			t.Fatalf("%s owner:group = %s:%s, want root:admin", path, r.owner, r.group)
		}
		if r.mode != 0o640 {
			t.Fatalf("%s mode = %#o, want 0640", path, r.mode)
		}
		if r.count < 1 {
			t.Fatalf("%s count = %d", path, r.count)
		}
		if r.sizeKB < 1 {
			t.Fatalf("%s size = %d KB, want a size limit", path, r.sizeKB)
		}
		if r.when != "*" {
			t.Fatalf("%s when = %q, want * so rotation is size-based", path, r.when)
		}
		if !strings.Contains(r.flags, "J") && !strings.Contains(r.flags, "Z") {
			t.Fatalf("%s flags = %q, want compression", path, r.flags)
		}
	}
}

type newsyslogRule struct {
	path   string
	owner  string
	group  string
	mode   int
	count  int
	sizeKB int
	when   string
	flags  string
}

func parseNewsyslog(b []byte) ([]newsyslogRule, error) {
	var rules []newsyslogRule
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if cut := strings.Index(line, " #"); cut >= 0 {
			line = strings.TrimSpace(line[:cut])
		}
		fields := strings.Fields(line)
		// path owner:group mode count size when flags
		if len(fields) != 7 {
			return nil, &newsyslogError{line: i + 1, msg: "want 7 fields (path owner:group mode count size when flags)"}
		}
		owner, group, ok := strings.Cut(fields[1], ":")
		if !ok || owner == "" || group == "" || strings.Contains(group, ":") {
			return nil, &newsyslogError{line: i + 1, msg: "owner:group must be name:name"}
		}
		mode, err := strconv.ParseInt(fields[2], 8, 32)
		if err != nil || mode < 0 || mode > 0o777 {
			return nil, &newsyslogError{line: i + 1, msg: "mode must be an octal file mode"}
		}
		count, err := strconv.Atoi(fields[3])
		if err != nil || count < 0 {
			return nil, &newsyslogError{line: i + 1, msg: "count must be a non-negative integer"}
		}
		size, err := strconv.Atoi(fields[4])
		if err != nil || size < 0 {
			return nil, &newsyslogError{line: i + 1, msg: "size must be kilobytes, not *"}
		}
		when := fields[5]
		flags := fields[6]
		for _, c := range flags {
			switch c {
			case 'B', 'C', 'D', 'G', 'J', 'N', 'U', 'Z', '-':
			default:
				return nil, &newsyslogError{line: i + 1, msg: "unknown flag " + string(c)}
			}
		}
		rules = append(rules, newsyslogRule{
			path: fields[0], owner: owner, group: group,
			mode: int(mode), count: count, sizeKB: size, when: when, flags: flags,
		})
	}
	if len(rules) == 0 {
		return nil, &newsyslogError{msg: "no rules"}
	}
	return rules, nil
}

type newsyslogError struct {
	line int
	msg  string
}

func (e *newsyslogError) Error() string {
	if e.line == 0 {
		return "newsyslog: " + e.msg
	}
	return "newsyslog line " + strconv.Itoa(e.line) + ": " + e.msg
}

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func parseLaunchdPlist(b []byte) (args []string, env map[string]string, ints map[string]int, top map[string]string, err error) {
	dec := xml.NewDecoder(strings.NewReader(string(b)))
	env = map[string]string{}
	ints = map[string]int{}
	top = map[string]string{}
	var stack []string
	var lastKey string
	var envDepth int
	inEnv := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return args, env, ints, top, nil
		}
		if err != nil {
			return nil, nil, nil, nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			stack = append(stack, t.Name.Local)
			if t.Name.Local == "dict" && lastKey == "EnvironmentVariables" {
				inEnv = true
				envDepth = len(stack)
			}
		case xml.EndElement:
			if inEnv && t.Name.Local == "dict" && len(stack) == envDepth {
				inEnv = false
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			s := strings.TrimSpace(string(t))
			if s == "" || len(stack) == 0 {
				continue
			}
			elem := stack[len(stack)-1]
			if elem == "key" {
				lastKey = s
				continue
			}
			parent := ""
			if len(stack) >= 2 {
				parent = stack[len(stack)-2]
			}
			if elem == "integer" && !inEnv && parent == "dict" {
				n, nerr := strconv.Atoi(s)
				if nerr != nil {
					return nil, nil, nil, nil, nerr
				}
				ints[lastKey] = n
				continue
			}
			if elem == "string" {
				if parent == "array" && lastKey == "ProgramArguments" {
					args = append(args, s)
				}
				if inEnv && parent == "dict" && len(stack) == envDepth+1 {
					env[lastKey] = s
				}
				// plist / dict / string. Nested dicts (environment) are deeper.
				if !inEnv && parent == "dict" && len(stack) == 3 {
					top[lastKey] = s
				}
			}
		}
	}
}
