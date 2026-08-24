package config

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestTLSConfigRequiresPaths(t *testing.T) {
	if _, err := TLSConfig("", "k", ""); err == nil {
		t.Fatal("empty cert")
	}
	if _, err := TLSConfig("c", "", ""); err == nil {
		t.Fatal("empty key")
	}
	if _, err := TLSConfig("/no/such/cert", "/no/such/key", ""); err == nil {
		t.Fatal("missing files")
	}
}

func TestTLSConfigClientAuth(t *testing.T) {
	dir := t.TempDir()
	f := writeTestCerts(t, dir)

	noCA, err := TLSConfig(f.serverCert, f.serverKey, "")
	if err != nil {
		t.Fatal(err)
	}
	if noCA.ClientAuth != tls.NoClientCert || noCA.ClientCAs != nil {
		t.Fatalf("client auth %+v", noCA.ClientAuth)
	}
	if len(noCA.Certificates) != 1 {
		t.Fatalf("certs %d", len(noCA.Certificates))
	}
	if noCA.MinVersion != tls.VersionTLS12 {
		t.Fatalf("min %d", noCA.MinVersion)
	}

	withCA, err := TLSConfig(f.serverCert, f.serverKey, f.clientCA)
	if err != nil {
		t.Fatal(err)
	}
	if withCA.ClientAuth != tls.RequireAndVerifyClientCert || withCA.ClientCAs == nil {
		t.Fatalf("client auth %+v", withCA.ClientAuth)
	}

	badCA := filepath.Join(dir, "bad-ca.pem")
	if err := os.WriteFile(badCA, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := TLSConfig(f.serverCert, f.serverKey, badCA); err == nil {
		t.Fatal("invalid client CA")
	}
	if _, err := TLSConfig(f.serverCert, f.serverKey, filepath.Join(dir, "missing-ca.pem")); err == nil {
		t.Fatal("missing client CA")
	}
}

func TestTLSConfigHandshake(t *testing.T) {
	dir := t.TempDir()
	f := writeTestCerts(t, dir)

	srv, err := TLSConfig(f.serverCert, f.serverKey, f.clientCA)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	roots := loadPool(t, f.serverCA)
	good, err := tls.LoadX509KeyPair(f.clientCert, f.clientKey)
	if err != nil {
		t.Fatal(err)
	}
	if cerr, serr := handshakePair(ln, &tls.Config{
		Certificates: []tls.Certificate{good},
		RootCAs:      roots,
		ServerName:   "localhost",
	}); cerr != nil || serr != nil {
		t.Fatalf("trusted client client=%v server=%v", cerr, serr)
	}

	if _, serr := handshakePair(ln, &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
	}); serr == nil {
		t.Fatal("expected missing client cert to fail")
	}

	other, err := tls.LoadX509KeyPair(f.otherCert, f.otherKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, serr := handshakePair(ln, &tls.Config{
		Certificates: []tls.Certificate{other},
		RootCAs:      roots,
		ServerName:   "localhost",
	}); serr == nil {
		t.Fatal("expected untrusted client cert to fail")
	}
}

func TestTLSConfigHandshakeNoClientCA(t *testing.T) {
	dir := t.TempDir()
	f := writeTestCerts(t, dir)
	srv, err := TLSConfig(f.serverCert, f.serverKey, "")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", srv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	roots := loadPool(t, f.serverCA)
	if cerr, serr := handshakePair(ln, &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
	}); cerr != nil || serr != nil {
		t.Fatalf("anonymous tls client=%v server=%v", cerr, serr)
	}
}

type testCertFiles struct {
	serverCert string
	serverKey  string
	serverCA   string
	clientCA   string
	clientCert string
	clientKey  string
	otherCert  string
	otherKey   string
}

func writeTestCerts(t *testing.T, dir string) testCertFiles {
	t.Helper()
	serverCA, serverCAKey, serverCADER := makeCA(t, "server-ca")
	serverKey, serverDER := makeLeaf(t, "localhost", serverCA, serverCAKey, false, true)

	clientCA, clientCAKey, clientCADER := makeCA(t, "client-ca")
	clientKey, clientDER := makeLeaf(t, "system:apiserver", clientCA, clientCAKey, true, false)

	otherCA, otherCAKey, _ := makeCA(t, "other-ca")
	otherKey, otherDER := makeLeaf(t, "intruder", otherCA, otherCAKey, true, false)

	return testCertFiles{
		serverCert: writePEM(t, dir, "server.crt", "CERTIFICATE", serverDER),
		serverKey:  writeKey(t, dir, "server.key", serverKey),
		serverCA:   writePEM(t, dir, "server-ca.crt", "CERTIFICATE", serverCADER),
		clientCA:   writePEM(t, dir, "client-ca.crt", "CERTIFICATE", clientCADER),
		clientCert: writePEM(t, dir, "client.crt", "CERTIFICATE", clientDER),
		clientKey:  writeKey(t, dir, "client.key", clientKey),
		otherCert:  writePEM(t, dir, "other.crt", "CERTIFICATE", otherDER),
		otherKey:   writeKey(t, dir, "other.key", otherKey),
	}
}

func makeCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey, []byte) {
	t.Helper()
	key := mustKey(t)
	tmpl := &x509.Certificate{
		SerialNumber:          serial(t),
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"darwin-node-test"}},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return parsed, key, der
}

func makeLeaf(t *testing.T, cn string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, client, server bool) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key := mustKey(t)
	tmpl := &x509.Certificate{
		SerialNumber: serial(t),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	if client {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
	}
	if server {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	return key, der
}

func mustKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func serial(t *testing.T) *big.Int {
	t.Helper()
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func writePEM(t *testing.T, dir, name, typ string, der []byte) string {
	t.Helper()
	p := filepath.Join(dir, name)
	b := pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der})
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeKey(t *testing.T, dir, name string, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return writePEM(t, dir, name, "PRIVATE KEY", der)
}

func loadPool(t *testing.T, path string) *x509.CertPool {
	t.Helper()
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		t.Fatalf("parse CA %s", path)
	}
	return pool
}

func handshakePair(ln net.Listener, clientCfg *tls.Config) (clientErr, serverErr error) {
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer c.Close()
		tc, ok := c.(*tls.Conn)
		if !ok {
			done <- fmt.Errorf("accept %T", c)
			return
		}
		done <- tc.Handshake()
	}()
	d := &tls.Dialer{Config: clientCfg}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := d.DialContext(ctx, "tcp", ln.Addr().String())
	if conn != nil {
		_ = conn.Close()
	}
	select {
	case serr := <-done:
		return err, serr
	case <-ctx.Done():
		return err, ctx.Err()
	}
}
