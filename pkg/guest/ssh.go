package guest

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
	"unicode"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// SSHOpts is last-resort guest access when the agent is not installed.
// Password authentication is not supported.
type SSHOpts struct {
	User           string
	KeyPEM         []byte
	KeyPath        string
	Host           string
	Port           int
	DialTimeout    time.Duration
	KnownHostsPath string
	HostKey        ssh.PublicKey // pinned key; takes precedence over KnownHostsPath
}

func (o SSHOpts) clientConfig() (*ssh.ClientConfig, error) {
	if o.User == "" {
		return nil, fmt.Errorf("ssh user required")
	}
	var auths []ssh.AuthMethod
	if len(o.KeyPEM) == 0 && o.KeyPath != "" {
		b, err := os.ReadFile(o.KeyPath)
		if err != nil {
			return nil, err
		}
		o.KeyPEM = b
	}
	if len(o.KeyPEM) > 0 {
		s, err := ssh.ParsePrivateKey(o.KeyPEM)
		if err != nil {
			return nil, fmt.Errorf("ssh key: %w", err)
		}
		auths = append(auths, ssh.PublicKeys(s))
	}
	if len(auths) == 0 {
		return nil, fmt.Errorf("ssh: no private key configured")
	}
	cb, err := hostKeyCallback(o)
	if err != nil {
		return nil, err
	}
	return &ssh.ClientConfig{
		User:            o.User,
		Auth:            auths,
		HostKeyCallback: cb,
		Timeout:         o.DialTimeout,
	}, nil
}

func hostKeyCallback(o SSHOpts) (ssh.HostKeyCallback, error) {
	if o.HostKey != nil {
		return ssh.FixedHostKey(o.HostKey), nil
	}
	if o.KnownHostsPath == "" {
		return nil, fmt.Errorf("ssh: refusing to connect without known_hosts or a pinned host key")
	}
	return knownhosts.New(o.KnownHostsPath)
}

// ParseHostKey parses an OpenSSH authorized_keys line or a raw base64 public key.
func ParseHostKey(s string) (ssh.PublicKey, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("ssh: empty host key")
	}
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(s))
	if err == nil {
		return pk, nil
	}
	raw, berr := base64.StdEncoding.DecodeString(s)
	if berr != nil {
		return nil, fmt.Errorf("ssh host key: %w", err)
	}
	pk, perr := ssh.ParsePublicKey(raw)
	if perr != nil {
		return nil, fmt.Errorf("ssh host key: %w", err)
	}
	return pk, nil
}

// SSHExec runs argv over SSH. Used only when the guest agent is unreachable.
func SSHExec(ctx context.Context, opts SSHOpts, argv []string) (stdout, stderr []byte, exit int, err error) {
	if opts.Port == 0 {
		opts.Port = 22
	}
	if opts.DialTimeout == 0 {
		opts.DialTimeout = 5 * time.Second
	}
	cfg, err := opts.clientConfig()
	if err != nil {
		return nil, nil, -1, err
	}
	d := net.Dialer{Timeout: opts.DialTimeout}
	addr := net.JoinHostPort(opts.Host, fmt.Sprintf("%d", opts.Port))
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, nil, -1, err
	}
	c, chans, reqs, err := ssh.NewClientConn(raw, addr, cfg)
	if err != nil {
		_ = raw.Close()
		return nil, nil, -1, err
	}
	cli := ssh.NewClient(c, chans, reqs)
	defer cli.Close()
	sess, err := cli.NewSession()
	if err != nil {
		return nil, nil, -1, err
	}
	defer sess.Close()
	var outBuf, errBuf bytes.Buffer
	sess.Stdout = &outBuf
	sess.Stderr = &errBuf
	cmd := joinArgv(argv)
	done := make(chan error, 1)
	go func() {
		done <- sess.Run(cmd)
	}()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGTERM)
		return outBuf.Bytes(), errBuf.Bytes(), -1, ctx.Err()
	case runErr := <-done:
		if ee, ok := runErr.(*ssh.ExitError); ok {
			return outBuf.Bytes(), errBuf.Bytes(), ee.ExitStatus(), nil
		}
		return outBuf.Bytes(), errBuf.Bytes(), 0, runErr
	}
}

func joinArgv(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = unixQuote(a)
	}
	return strings.Join(parts, " ")
}

func unixQuote(s string) string {
	if s == "" {
		return "''"
	}
	if isSafeUnixArg(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func isSafeUnixArg(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			continue
		}
		switch r {
		case '/', '.', '-', '_', '=', ':', '+', ',', '@', '%':
			continue
		default:
			return false
		}
	}
	return true
}
