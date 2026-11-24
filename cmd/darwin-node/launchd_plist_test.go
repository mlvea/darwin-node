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

	"github.com/darwin-node/darwin-node/pkg/config"
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
	args, env, _, err := parseLaunchdPlist(b)
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
	_, _, ints, err := parseLaunchdPlist(b)
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

func flagValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func parseLaunchdPlist(b []byte) (args []string, env map[string]string, ints map[string]int, err error) {
	dec := xml.NewDecoder(strings.NewReader(string(b)))
	env = map[string]string{}
	ints = map[string]int{}
	var stack []string
	var lastKey string
	var envDepth int
	inEnv := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return args, env, ints, nil
		}
		if err != nil {
			return nil, nil, nil, err
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
			top := stack[len(stack)-1]
			if top == "key" {
				lastKey = s
				continue
			}
			parent := ""
			if len(stack) >= 2 {
				parent = stack[len(stack)-2]
			}
			if top == "integer" && !inEnv && parent == "dict" {
				n, nerr := strconv.Atoi(s)
				if nerr != nil {
					return nil, nil, nil, nerr
				}
				ints[lastKey] = n
				continue
			}
			if top != "string" {
				continue
			}
			if parent == "array" && lastKey == "ProgramArguments" {
				args = append(args, s)
			}
			if inEnv && parent == "dict" && len(stack) == envDepth+1 {
				env[lastKey] = s
			}
		}
	}
}
