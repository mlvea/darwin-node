package guest

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestJoinArgvQuotesSpaces(t *testing.T) {
	got := joinArgv([]string{"echo", "foo bar"})
	if got != "echo 'foo bar'" && !strings.Contains(got, `'foo bar'`) {
		t.Fatalf("got %q", got)
	}
	if joinArgv([]string{"true"}) != "true" {
		t.Fatal(joinArgv([]string{"true"}))
	}
}

func TestJoinArgvMetacharactersStayQuoted(t *testing.T) {
	argv := []string{
		"cat",
		"/etc/hosts; rm -rf ~",
		"true;rm",
		"$(reboot)",
		"`id`",
		"line1\nline2",
		`it's "quoted"`,
	}
	cmd := joinArgv(argv)
	got := splitUnixQuoted(cmd)
	if len(got) != len(argv) {
		t.Fatalf("word count %d want %d cmd=%q got=%q", len(got), len(argv), cmd, got)
	}
	for i := range argv {
		if got[i] != argv[i] {
			t.Fatalf("arg %d: got %q want %q (cmd=%q)", i, got[i], argv[i], cmd)
		}
		q := unixQuote(argv[i])
		if !strings.Contains(cmd, q) {
			t.Fatalf("quoted form %q not in %q", q, cmd)
		}
	}
	for _, needle := range []string{";", "$(", "`", "\n", `"`} {
		idx := strings.Index(cmd, needle)
		if idx < 0 {
			t.Fatalf("cmd %q missing %q", cmd, needle)
		}
		if !insideSingleQuotes(cmd, idx) {
			t.Fatalf("%q not inside quotes in %q", needle, cmd)
		}
	}
	if unixQuote("true;rm") != "'true;rm'" {
		t.Fatalf("semicolon must be quoted: %q", unixQuote("true;rm"))
	}
}

func FuzzJoinArgvRoundtrip(f *testing.F) {
	f.Add("echo", "foo bar")
	f.Add("cat", "true; rm -rf /")
	f.Add("x", "$(id)")
	f.Add("x", "`id`")
	f.Add("x", "a\nb")
	f.Add("x", `it's`)
	f.Add("", "")
	f.Fuzz(func(t *testing.T, a, b string) {
		argv := []string{a, b}
		cmd := joinArgv(argv)
		got := splitUnixQuoted(cmd)
		if len(got) != 2 || got[0] != a || got[1] != b {
			t.Fatalf("argv=%q cmd=%q got=%q", argv, cmd, got)
		}
	})
}

func TestHostKeyCallbackRequiresPinOrKnownHosts(t *testing.T) {
	_, err := hostKeyCallback(SSHOpts{})
	if err == nil {
		t.Fatal("expected error when neither pin nor known_hosts is set")
	}
	if !strings.Contains(err.Error(), "known_hosts") && !strings.Contains(err.Error(), "pinned") {
		t.Fatalf("err %v", err)
	}
}

func TestHostKeyCallbackKnownHostsFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "known_hosts")
	if err := os.WriteFile(p, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	cb, err := hostKeyCallback(SSHOpts{KnownHostsPath: p})
	if err != nil {
		t.Fatal(err)
	}
	if cb == nil {
		t.Fatal("nil callback")
	}
}

func TestHostKeyCallbackPinned(t *testing.T) {
	pk := testPublicKey(t)
	cb, err := hostKeyCallback(SSHOpts{HostKey: pk})
	if err != nil {
		t.Fatal(err)
	}
	if cb == nil {
		t.Fatal("nil callback")
	}
}

func TestClientConfigRequiresKeyNotPassword(t *testing.T) {
	_, err := SSHOpts{User: "admin", KnownHostsPath: "/tmp/kh"}.clientConfig()
	if err == nil || !strings.Contains(err.Error(), "key") {
		t.Fatalf("got %v", err)
	}
	if err != nil && strings.Contains(err.Error(), "password") {
		t.Fatalf("password auth must stay retired: %v", err)
	}
}

func TestParseHostKeyAuthorizedAndRaw(t *testing.T) {
	pk := testPublicKey(t)
	line := string(ssh.MarshalAuthorizedKey(pk))
	got, err := ParseHostKey(line)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type() != pk.Type() {
		t.Fatalf("type %s want %s", got.Type(), pk.Type())
	}
	raw := base64.StdEncoding.EncodeToString(pk.Marshal())
	got, err = ParseHostKey(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.Type() != pk.Type() {
		t.Fatalf("raw type %s", got.Type())
	}
	if _, err := ParseHostKey(""); err == nil {
		t.Fatal("empty")
	}
}

func testPublicKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s.PublicKey()
}

// splitUnixQuoted inverts joinArgv / unixQuote (single quotes and '"'"' embeds).
func splitUnixQuoted(cmd string) []string {
	var words []string
	i := 0
	n := len(cmd)
	for i < n {
		if cmd[i] == ' ' {
			i++
			continue
		}
		var b strings.Builder
		for i < n && cmd[i] != ' ' {
			if cmd[i] != '\'' {
				b.WriteByte(cmd[i])
				i++
				continue
			}
			i++
			for i < n {
				if cmd[i] != '\'' {
					b.WriteByte(cmd[i])
					i++
					continue
				}
				i++
				if i+4 <= n && cmd[i] == '"' && cmd[i+1] == '\'' && cmd[i+2] == '"' && cmd[i+3] == '\'' {
					b.WriteByte('\'')
					i += 4
					continue
				}
				break
			}
		}
		words = append(words, b.String())
	}
	return words
}

func insideSingleQuotes(cmd string, idx int) bool {
	in := false
	i := 0
	for i < len(cmd) && i <= idx {
		if cmd[i] != '\'' {
			i++
			continue
		}
		if in && i+4 <= len(cmd) && cmd[i+1] == '"' && cmd[i+2] == '\'' && cmd[i+3] == '"' && cmd[i+4] == '\'' {
			i += 5
			continue
		}
		in = !in
		i++
	}
	return in
}
