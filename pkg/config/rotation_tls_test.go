package config

import (
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"strings"
	"testing"
	"time"
)

type memProvider struct {
	cur  *tls.Certificate
	kept *tls.Certificate
}

func (m memProvider) Current() *tls.Certificate { return m.cur }

func (m *memProvider) Retained() (*tls.Certificate, error) { return m.kept, nil }

func TestTLSConfigFromSourceNilMatchesFileReload(t *testing.T) {
	dir := t.TempDir()
	ca, caKey, caDER := makeCA(t, "server-ca")
	key, der := makeLeaf(t, "first", ca, caKey, false, true)
	certPath := writePEM(t, dir, "server.crt", "CERTIFICATE", der)
	keyPath := writeKey(t, dir, "server.key", key)
	caPath := writePEM(t, dir, "server-ca.crt", "CERTIFICATE", caDER)
	roots := loadPool(t, caPath)

	fileCfg, err := TLSConfig(certPath, keyPath, "")
	if err != nil {
		t.Fatal(err)
	}
	same, err := TLSConfigFromSource(certPath, keyPath, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if cn := servedCN(t, fileCfg, roots); cn != "first" {
		t.Fatalf("file config CN %q", cn)
	}
	if cn := servedCN(t, same, roots); cn != "first" {
		t.Fatalf("nil source CN %q", cn)
	}
	beforeA := certDER(t, fileCfg)
	beforeB := certDER(t, same)
	if string(beforeA) != string(beforeB) || string(beforeA) != string(der) {
		t.Fatal("nil source served a different certificate than TLSConfig")
	}

	key2, der2 := makeLeaf(t, "rotated", ca, caKey, false, true)
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der2}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8(t, key2)}), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := os.Chtimes(certPath, future, future); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(keyPath, future, future); err != nil {
		t.Fatal(err)
	}
	afterA := certDER(t, fileCfg)
	afterB := certDER(t, same)
	if string(afterA) != string(der2) || string(afterB) != string(der2) {
		t.Fatal("nil source did not follow the file reload")
	}
}

func TestTLSConfigFromSourceIgnoresFiles(t *testing.T) {
	dir := t.TempDir()
	ca, caKey, _ := makeCA(t, "server-ca")
	fileKey, fileDER := makeLeaf(t, "from-file", ca, caKey, false, true)
	certPath := writePEM(t, dir, "server.crt", "CERTIFICATE", fileDER)
	keyPath := writeKey(t, dir, "server.key", fileKey)
	mgrKey, mgrDER := makeLeaf(t, "from-manager", ca, caKey, false, true)
	keptKey, keptDER := makeLeaf(t, "retained", ca, caKey, false, true)
	src := &memProvider{cur: leafPair(t, mgrKey, mgrDER), kept: leafPair(t, keptKey, keptDER)}

	cfg, err := TLSConfigFromSource(certPath, keyPath, "", src)
	if err != nil {
		t.Fatal(err)
	}
	if got := certDER(t, cfg); string(got) != string(mgrDER) {
		t.Fatal("GetCertificate did not return the manager certificate")
	}
	otherKey, otherDER := makeLeaf(t, "rewritten", ca, caKey, false, true)
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: otherDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8(t, otherKey)}), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Hour)
	if err := os.Chtimes(certPath, later, later); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(keyPath, later, later); err != nil {
		t.Fatal(err)
	}
	if got := certDER(t, cfg); string(got) != string(mgrDER) {
		t.Fatal("rotation path followed the file mtime")
	}
	src.cur = nil
	if got := certDER(t, cfg); string(got) != string(keptDER) {
		t.Fatal("nil current certificate did not fall back to the retained certificate")
	}
}

func TestRequireKubeletTLSRotationSkipsFiles(t *testing.T) {
	if err := RequireKubeletTLS(false, Config{RotateServerCertificates: true}); err != nil {
		t.Fatal(err)
	}
	if err := RequireKubeletTLS(false, Config{RotateClientCertificates: true}); err == nil {
		t.Fatal("client rotation alone skipped the serving files")
	}
}

func TestRotationEnvAndValidate(t *testing.T) {
	t.Setenv("DARWIN_NODE_SSH_PASSWORD", "")
	t.Setenv("VZ_SSH_PASSWORD", "")
	t.Setenv("DARWIN_NODE_ROTATE_SERVER_CERTIFICATES", "1")
	t.Setenv("DARWIN_NODE_ROTATE_CLIENT_CERTIFICATES", "false")
	t.Setenv("DARWIN_NODE_BOOTSTRAP_KUBECONFIG", "/var/lib/boot")
	t.Setenv("DARWIN_NODE_CERT_DIR", "/var/lib/pki")
	c := Default()
	if c.RotateServerCertificates || c.RotateClientCertificates {
		t.Fatal("defaults enable rotation")
	}
	if c.CertDir == "" {
		t.Fatal("default cert dir is empty")
	}
	if err := c.MergeEnv(); err != nil {
		t.Fatal(err)
	}
	if !c.RotateServerCertificates || c.RotateClientCertificates {
		t.Fatalf("server %v client %v", c.RotateServerCertificates, c.RotateClientCertificates)
	}
	if c.BootstrapKubeconfig != "/var/lib/boot" || c.CertDir != "/var/lib/pki" {
		t.Fatalf("boot %q dir %q", c.BootstrapKubeconfig, c.CertDir)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("DARWIN_NODE_ROTATE_SERVER_CERTIFICATES", "maybe")
	c = Default()
	if err := c.MergeEnv(); err == nil || !strings.Contains(err.Error(), "DARWIN_NODE_ROTATE_SERVER_CERTIFICATES") {
		t.Fatalf("bad bool: %v", err)
	}

	c = Default()
	c.RotateClientCertificates = true
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "bootstrap-kubeconfig") {
		t.Fatalf("missing bootstrap: %v", err)
	}
	c.BootstrapKubeconfig = "/boot"
	c.CertDir = " "
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "cert-dir") {
		t.Fatalf("missing cert dir: %v", err)
	}
	c = Default()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func servedCN(t *testing.T, cfg *tls.Config, roots *x509.CertPool) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go acceptHandshakes(ln)
	cn, err := dialServerCN(ln.Addr().String(), roots)
	if err != nil {
		t.Fatal(err)
	}
	return cn
}

func certDER(t *testing.T, cfg *tls.Config) []byte {
	t.Helper()
	c, err := cfg.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil {
		t.Fatal(err)
	}
	if c == nil || len(c.Certificate) == 0 {
		t.Fatal("no certificate")
	}
	return c.Certificate[0]
}

func leafPair(t *testing.T, key *ecdsa.PrivateKey, der []byte) *tls.Certificate {
	t.Helper()
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

func pkcs8(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}
