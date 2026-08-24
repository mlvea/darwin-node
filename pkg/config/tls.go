package config

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
)

// RequireKubeletTLS refuses a plaintext kubelet HTTP server in cluster mode.
// --standalone does not serve that API and does not need certs.
func RequireKubeletTLS(standalone bool, cfg Config) error {
	if standalone {
		return nil
	}
	if strings.TrimSpace(cfg.CertPath) == "" || strings.TrimSpace(cfg.KeyPath) == "" {
		return fmt.Errorf("kubelet TLS cert and key are required unless --standalone (set APISERVER_CERT_LOCATION and APISERVER_KEY_LOCATION)")
	}
	return nil
}

// TLSConfig loads the kubelet HTTP server certificate and, when clientCA is
// set, a client CA pool with RequireAndVerifyClientCert.
func TLSConfig(cert, key, clientCA string) (*tls.Config, error) {
	if strings.TrimSpace(cert) == "" || strings.TrimSpace(key) == "" {
		return nil, fmt.Errorf("tls cert and key paths are required")
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("load kubelet tls cert: %w", err)
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.NoClientCert,
	}
	if strings.TrimSpace(clientCA) == "" {
		return cfg, nil
	}
	pem, err := os.ReadFile(clientCA)
	if err != nil {
		return nil, fmt.Errorf("read client CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("parse client CA %q: no certificates", clientCA)
	}
	cfg.ClientCAs = pool
	cfg.ClientAuth = tls.RequireAndVerifyClientCert
	return cfg, nil
}
