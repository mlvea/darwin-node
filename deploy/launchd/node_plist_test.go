package launchd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNodeAgentLaunchdAbandonsGroupAndStaysInteractive(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	path := filepath.Join(filepath.Dir(file), "io.darwin-node.node.plist")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !plistBool(text, "AbandonProcessGroup") {
		t.Fatal("node agent plist must set AbandonProcessGroup true so launchd does not signal supervisor sessions")
	}
	if got := plistString(text, "ProcessType"); got != "Interactive" {
		t.Fatalf("ProcessType %q, want Interactive so App Nap does not suspend the agent", got)
	}
}

func plistValue(text, key string) string {
	needle := "<key>" + key + "</key>"
	idx := strings.Index(text, needle)
	if idx < 0 {
		return ""
	}
	rest := text[idx+len(needle):]
	if next := strings.Index(rest, "<key>"); next >= 0 {
		rest = rest[:next]
	}
	return rest
}

func plistBool(text, key string) bool {
	return strings.Contains(plistValue(text, key), "<true/>")
}

func plistString(text, key string) string {
	rest := plistValue(text, key)
	open := strings.Index(rest, "<string>")
	close := strings.Index(rest, "</string>")
	if open < 0 || close < 0 || close < open {
		return ""
	}
	return rest[open+len("<string>") : close]
}
