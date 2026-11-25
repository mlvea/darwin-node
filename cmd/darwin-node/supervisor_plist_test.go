package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSlotLaunchdPlists(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	dir := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "launchd")
	for _, slot := range []string{"0", "1"} {
		path := filepath.Join(dir, "io.darwin-node.slot-"+slot+".plist")
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(body)
		for _, want := range []string{
			"<string>io.darwin-node.slot-" + slot + "</string>",
			"<string>--role=supervisor</string>",
			"<string>--slot=" + slot + "</string>",
			"<string>--runtime=vz</string>",
			"<string>--vm-supervisor-run-dir=/var/lib/darwin-node/run</string>",
			"<key>SuccessfulExit</key>",
			"<false/>",
			"<key>RunAtLoad</key>",
			"<true/>",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s missing %s", path, want)
			}
		}
		if strings.Contains(text, "<key>Sockets</key>") {
			t.Errorf("%s must bind its own socket", path)
		}
	}
}
