package engine

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/darwin-node/darwin-node/pkg/config"

	"golang.org/x/crypto/ssh"
)

func TestDecodeSSHKeyB64(t *testing.T) {
	raw := []byte("-----BEGIN OPENSSH PRIVATE KEY-----\ntest\n")
	enc := base64.StdEncoding.EncodeToString(raw)
	got := decodeSSHKeyB64(enc)
	if string(got) != string(raw) {
		t.Fatalf("got %q", got)
	}
	if decodeSSHKeyB64("") != nil {
		t.Fatal("empty")
	}
}

func TestSSHOptsFromConfigUsesBase64PEM(t *testing.T) {
	cfg := config.Default()
	cfg.SSHUser = "admin"
	cfg.SSHPassword = "must-not-be-used"
	cfg.SSHPrivateKeyB64 = base64.StdEncoding.EncodeToString([]byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"))
	cfg.SSHKnownHosts = "/tmp/known_hosts"
	opts := sshOptsFromConfig(cfg, "192.168.64.2")
	if opts.User != "admin" || opts.Host != "192.168.64.2" {
		t.Fatalf("%+v", opts)
	}
	if len(opts.KeyPEM) == 0 {
		t.Fatal("KeyPEM empty; base64 env was ignored")
	}
	if opts.KnownHostsPath != "/tmp/known_hosts" {
		t.Fatalf("known_hosts %s", opts.KnownHostsPath)
	}
}

func TestSSHFallbackEnabled(t *testing.T) {
	cfg := config.Default()
	if SSHFallbackEnabled(cfg) {
		t.Fatal("default must be off")
	}
	cfg.SSHUser = "admin"
	if SSHFallbackEnabled(cfg) {
		t.Fatal("user without opt-in must stay off")
	}
	cfg.EnableSSHFallback = true
	if !SSHFallbackEnabled(cfg) {
		t.Fatal("want enabled")
	}
	cfg.SSHUser = ""
	if SSHFallbackEnabled(cfg) {
		t.Fatal("opt-in without user must stay off")
	}
}

func TestSSHOptsFromConfigPinsHostKey(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	line := string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
	cfg := config.Default()
	cfg.SSHUser = "admin"
	cfg.SSHHostKey = line
	opts := sshOptsFromConfig(cfg, "192.168.64.2")
	if opts.HostKey == nil {
		t.Fatal("expected pinned host key")
	}
}

func TestExecInVMSSHFallbackGated(t *testing.T) {
	e := testEngine(t)
	e.mu.Lock()
	e.pods["default/x"] = &podRecord{pod: samplePod("x", "uid-x")}
	e.mu.Unlock()
	ctx := context.Background()
	err := e.ExecInVM(ctx, "default", "x", []string{"true; rm -rf /"}, nil)
	if err == nil || err.Error() != "guest agent not connected" {
		t.Fatalf("default: %v", err)
	}
	e.cfg.SSHUser = "admin"
	err = e.ExecInVM(ctx, "default", "x", []string{"true"}, nil)
	if err == nil || err.Error() != "guest agent not connected" {
		t.Fatalf("user without opt-in: %v", err)
	}
	e.cfg.EnableSSHFallback = true
	err = e.ExecInVM(ctx, "default", "x", []string{"true"}, nil)
	if err == nil || (!strings.Contains(err.Error(), "known_hosts") && !strings.Contains(err.Error(), "pinned")) {
		t.Fatalf("missing host key: %v", err)
	}
}
