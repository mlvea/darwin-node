package config

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"
	"sync/atomic"

	"k8s.io/apiserver/pkg/server/dynamiccertificates"
)

// ClientCABundle is the kubelet client-certificate CA loaded from a file.
//
// Call Run so a replacement file is picked up. InstallTLS refreshes the
// handshake's ClientCAs from that same bundle on every connection.
// Webhook auth should use Content so its verifier follows the same bytes.
// An empty or unparsable replacement leaves the last good bundle in place.
type ClientCABundle struct {
	file     *dynamiccertificates.DynamicFileCAContent
	lastGood atomic.Pointer[x509.CertPool]
}

// NewClientCABundle reads path. An empty, missing, or unparsable file is
// an error so startup stays fail-closed. The returned bundle does not
// watch the file until Run.
func NewClientCABundle(path string) (*ClientCABundle, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("client CA path is empty")
	}
	file, err := dynamiccertificates.NewDynamicCAContentFromFile("client-ca", path)
	if err != nil {
		return nil, fmt.Errorf("client CA %q: %w", path, err)
	}
	b := &ClientCABundle{file: file}
	b.pool()
	if b.lastGood.Load() == nil {
		return nil, fmt.Errorf("client CA %q: no certificates", path)
	}
	return b, nil
}

// Run watches the file until ctx is cancelled. It blocks.
func (b *ClientCABundle) Run(ctx context.Context) {
	b.file.Run(ctx, 1)
}

// Content is the CA provider webhook auth verifies client certificates with.
func (b *ClientCABundle) Content() dynamiccertificates.CAContentProvider {
	return b.file
}

// InstallTLS sets VerifyClientCertIfGiven and a GetConfigForClient hook
// that copies ClientCAs from the live bundle. Certificates already on cfg
// are left in place and cloned into each handshake.
func (b *ClientCABundle) InstallTLS(cfg *tls.Config) {
	cfg.ClientAuth = tls.VerifyClientCertIfGiven
	cfg.ClientCAs = b.pool()
	cfg.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
		next := cfg.Clone()
		// The clone would otherwise call this hook again if a caller
		// re-enters it. crypto/tls uses the returned config as-is.
		next.GetConfigForClient = nil
		next.ClientAuth = tls.VerifyClientCertIfGiven
		next.ClientCAs = b.pool()
		return next, nil
	}
}

// pool parses the in-memory bundle. A bad parse returns the previous
// pool so a torn read cannot fall through to the system roots.
func (b *ClientCABundle) pool() *x509.CertPool {
	pemBytes := b.file.CurrentCABundleContent()
	next := x509.NewCertPool()
	if len(pemBytes) == 0 || !next.AppendCertsFromPEM(pemBytes) {
		if prev := b.lastGood.Load(); prev != nil {
			return prev
		}
		return x509.NewCertPool()
	}
	b.lastGood.Store(next)
	return next
}

// reload reads the file once. The watcher inside Run uses the same
// loader. An error means the previous bundle was kept.
func (b *ClientCABundle) reload(ctx context.Context) error {
	return b.file.RunOnce(ctx)
}
