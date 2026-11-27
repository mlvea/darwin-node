package config

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// RequireKubeletTLS refuses a plaintext kubelet HTTP server in cluster mode.
// --standalone does not serve that API and does not need certs.
// --rotate-server-certificates supplies the certificate from the manager,
// so the static files are not required while that flag is on.
func RequireKubeletTLS(standalone bool, cfg Config) error {
	if standalone || cfg.RotateServerCertificates {
		return nil
	}
	if strings.TrimSpace(cfg.CertPath) == "" || strings.TrimSpace(cfg.KeyPath) == "" {
		return fmt.Errorf("kubelet TLS cert and key are required unless --standalone (set APISERVER_CERT_LOCATION and APISERVER_KEY_LOCATION)")
	}
	return nil
}

// RequireKubeletClientCA refuses cluster mode when no client CA is configured.
// Without that CA the kubelet handler is anonymous after TLS, so anyone who
// can reach the listen port can exec into a VM. --standalone does not serve
// that API. Whitespace is unset.
func RequireKubeletClientCA(standalone bool, clientCA string) error {
	if standalone {
		return nil
	}
	if strings.TrimSpace(clientCA) == "" {
		return fmt.Errorf("cluster mode refuses to start without a kubelet client CA (set APISERVER_CA_CERT_LOCATION). Without that CA the exec/logs API is anonymous after TLS. --standalone does not serve that API")
	}
	return nil
}

// TLSConfig loads the kubelet HTTP server certificate and, when clientCA is
// set, a client CA pool. Presented client certificates are verified
// (tls.VerifyClientCertIfGiven). A connection that presents no certificate
// still completes the handshake so the handler can run bearer TokenReview.
// RequireAndVerifyClientCert would reject those callers during the handshake,
// and WebhookAuth's x509 authenticator would then never see a token.
//
// The pool is a snapshot of the file at call time. A CA that must rotate
// without a restart is a ClientCABundle: Run watches the file, and
// InstallTLS refreshes ClientCAs from that bundle on each handshake.
//
// The serving certificate is not copied into Certificates. GetCertificate
// reloads the files when the mtime or size changes, and a failed reload
// keeps the last pair that parsed. Certificates stays empty: a non-empty
// slice is what crypto/tls serves to a client that sends no SNI, which
// would pin the pair loaded at startup.
func TLSConfig(cert, key, clientCA string) (*tls.Config, error) {
	cert = strings.TrimSpace(cert)
	key = strings.TrimSpace(key)
	if cert == "" || key == "" {
		return nil, fmt.Errorf("tls cert and key paths are required")
	}
	src := &servingCert{certPath: cert, keyPath: key}
	if _, err := src.get(nil); err != nil {
		return nil, err
	}
	cfg := &tls.Config{
		MinVersion:     tls.VersionTLS12,
		GetCertificate: src.get,
		ClientAuth:     tls.NoClientCert,
	}
	if err := applyClientCA(cfg, clientCA); err != nil {
		return nil, err
	}
	return cfg, nil
}

func applyClientCA(cfg *tls.Config, clientCA string) error {
	if strings.TrimSpace(clientCA) == "" {
		return nil
	}
	pemBytes, err := os.ReadFile(clientCA)
	if err != nil {
		return fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return fmt.Errorf("parse client CA %q: no certificates", clientCA)
	}
	cfg.ClientCAs = pool
	cfg.ClientAuth = tls.VerifyClientCertIfGiven
	return nil
}

// CertificateProvider is the serving certificate when rotation is on.
// Current hides an expired certificate. Retained still returns it.
type CertificateProvider interface {
	Current() *tls.Certificate
	Retained() (*tls.Certificate, error)
}

// TLSConfigFromSource is TLSConfig when src is nil. When src is set,
// GetCertificate returns the provider's current certificate, or the
// retained one when the current one is hidden, and does not read cert
// and key.
func TLSConfigFromSource(cert, key, clientCA string, src CertificateProvider) (*tls.Config, error) {
	if src == nil {
		return TLSConfig(cert, key, clientCA)
	}
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			if c := src.Current(); c != nil && len(c.Certificate) > 0 {
				return c, nil
			}
			kept, err := src.Retained()
			if err != nil {
				return nil, err
			}
			if kept == nil || len(kept.Certificate) == 0 {
				return nil, errors.New("no serving certificate available")
			}
			return kept, nil
		},
		ClientAuth: tls.NoClientCert,
	}
	if err := applyClientCA(cfg, clientCA); err != nil {
		return nil, err
	}
	return cfg, nil
}

// servingCert is the kubelet HTTP certificate. get is tls.Config.GetCertificate.
type servingCert struct {
	certPath string
	keyPath  string

	mu       sync.Mutex
	current  *tls.Certificate
	certMod  time.Time
	keyMod   time.Time
	certSize int64
	keySize  int64
}

func (s *servingCert) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ci, cerr := os.Stat(s.certPath)
	ki, kerr := os.Stat(s.keyPath)
	if cerr != nil || kerr != nil {
		if s.current != nil {
			return s.current, nil
		}
		if cerr != nil {
			return nil, fmt.Errorf("stat kubelet tls cert: %w", cerr)
		}
		return nil, fmt.Errorf("stat kubelet tls key: %w", kerr)
	}
	if s.current != nil && ci.ModTime().Equal(s.certMod) && ki.ModTime().Equal(s.keyMod) && ci.Size() == s.certSize && ki.Size() == s.keySize {
		return s.current, nil
	}
	pair, err := tls.LoadX509KeyPair(s.certPath, s.keyPath)
	if err != nil {
		if s.current != nil {
			return s.current, nil
		}
		return nil, fmt.Errorf("load kubelet tls cert: %w", err)
	}
	s.current = &pair
	s.certMod, s.keyMod = ci.ModTime(), ki.ModTime()
	s.certSize, s.keySize = ci.Size(), ki.Size()
	return s.current, nil
}
