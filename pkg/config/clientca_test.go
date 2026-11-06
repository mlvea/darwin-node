package config

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/klog/v2"
)

func TestMain(m *testing.M) {
	// The CA watcher logs through klog. Set the logger once, before any
	// controller starts: SetLogger is process-global and not safe against
	// a worker already inside klog.V.
	klog.SetLogger(logr.Discard())
	os.Exit(m.Run())
}

func TestClientCABundleFailClosed(t *testing.T) {
	if _, err := NewClientCABundle(""); err == nil {
		t.Fatal("empty path")
	}
	if _, err := NewClientCABundle(" \t"); err == nil {
		t.Fatal("whitespace path")
	}
	if _, err := NewClientCABundle(filepath.Join(t.TempDir(), "missing.pem")); err == nil {
		t.Fatal("missing file")
	}
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClientCABundle(bad); err == nil {
		t.Fatal("invalid file")
	}
}

func TestClientCAReloadServesRotatedBundle(t *testing.T) {
	dir := t.TempDir()
	f := writeTestCerts(t, dir)
	ca2, ca2Key, ca2DER := makeCA(t, "client-ca-2")
	leaf2Key, leaf2DER := makeLeaf(t, "rotated-client", ca2, ca2Key, true, false)
	ca2PEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca2DER})

	bundle := startClientCA(t, f.clientCA)
	srv := listenClientCA(t, f, bundle)
	roots := loadPool(t, f.serverCA)
	oldLeaf := mustLeafPair(t, f.clientCert, f.clientKey)
	newLeaf := tls.Certificate{Certificate: [][]byte{leaf2DER}, PrivateKey: leaf2Key}

	if cerr, serr := handshakePair(srv, clientAuthCfg(roots, oldLeaf)); cerr != nil || serr != nil {
		t.Fatalf("initial client client=%v server=%v", cerr, serr)
	}

	// Rewrite until the watcher observes the new file. A pinned pool never will.
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		replaceFile(t, f.clientCA, ca2PEM)
		cerr, serr := handshakePair(srv, clientAuthCfg(roots, newLeaf))
		if cerr == nil && serr == nil {
			last = nil
			break
		}
		last = fmt.Errorf("client=%v server=%v", cerr, serr)
		time.Sleep(30 * time.Millisecond)
	}
	if last != nil {
		t.Fatalf("rotated CA was not accepted: %v", last)
	}
	if _, serr := handshakePair(srv, clientAuthCfg(roots, oldLeaf)); serr == nil {
		t.Fatal("previous client CA still accepted after rotation")
	}
	opts, ok := bundle.Content().VerifyOptions()
	if !ok {
		t.Fatal("verify options missing after rotation")
	}
	parsed, err := x509.ParseCertificate(leaf2DER)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsed.Verify(opts); err != nil {
		t.Fatalf("authenticator bundle did not follow rotation: %v", err)
	}
	oldParsed := mustParseCert(t, f.clientCert)
	if _, err := oldParsed.Verify(opts); err == nil {
		t.Fatal("authenticator still trusts the previous CA")
	}
}

func TestClientCAInvalidReloadKeepsBundle(t *testing.T) {
	dir := t.TempDir()
	f := writeTestCerts(t, dir)
	bundle := startClientCA(t, f.clientCA)
	srv := listenClientCA(t, f, bundle)
	roots := loadPool(t, f.serverCA)
	good := mustLeafPair(t, f.clientCert, f.clientKey)
	other := mustLeafPair(t, f.otherCert, f.otherKey)

	if cerr, serr := handshakePair(srv, clientAuthCfg(roots, good)); cerr != nil || serr != nil {
		t.Fatalf("initial client client=%v server=%v", cerr, serr)
	}

	for _, body := range [][]byte{nil, []byte("not a certificate\n")} {
		replaceFile(t, f.clientCA, body)
		if err := bundle.reload(context.Background()); err == nil {
			t.Fatalf("reload of %q succeeded", body)
		}
		if cerr, serr := handshakePair(srv, clientAuthCfg(roots, good)); cerr != nil || serr != nil {
			t.Fatalf("good client unloaded after %q: client=%v server=%v", body, cerr, serr)
		}
		if _, serr := handshakePair(srv, clientAuthCfg(roots, other)); serr == nil {
			t.Fatalf("untrusted client accepted after %q", body)
		}
		opts, ok := bundle.Content().VerifyOptions()
		if !ok {
			t.Fatal("verify options dropped")
		}
		if _, err := mustParseCert(t, f.clientCert).Verify(opts); err != nil {
			t.Fatalf("authenticator bundle unloaded after %q: %v", body, err)
		}
	}
}

func startClientCA(t *testing.T, path string) *ClientCABundle {
	t.Helper()
	bundle, err := NewClientCABundle(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go bundle.Run(ctx)
	return bundle
}

func listenClientCA(t *testing.T, f testCertFiles, bundle *ClientCABundle) net.Listener {
	t.Helper()
	srv, err := TLSConfig(f.serverCert, f.serverKey, "")
	if err != nil {
		t.Fatal(err)
	}
	bundle.InstallTLS(srv)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func clientAuthCfg(roots *x509.CertPool, cert tls.Certificate) *tls.Config {
	return &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		// Certificates is filtered against the server's advertised CAs.
		// Forcing the cert keeps a rotated-out client on the wire so the
		// server can reject it instead of completing with no certificate.
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return &cert, nil
		},
	}
}

func mustLeafPair(t *testing.T, certPath, keyPath string) tls.Certificate {
	t.Helper()
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		t.Fatal(err)
	}
	return pair
}

func mustParseCert(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("no PEM in %s", path)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func replaceFile(t *testing.T, path string, body []byte) {
	t.Helper()
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}
