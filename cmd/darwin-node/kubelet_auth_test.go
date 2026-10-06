package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSelectKubeletAuth(t *testing.T) {
	if got := selectKubeletAuth(""); got != kubeletAuthNoAuth {
		t.Fatalf("empty CA: %q", got)
	}
	if got := selectKubeletAuth(" \t\n"); got != kubeletAuthNoAuth {
		t.Fatalf("whitespace CA: %q", got)
	}
	if got := selectKubeletAuth("/var/run/client-ca.pem"); got != kubeletAuthWebhook {
		t.Fatalf("set CA: %q", got)
	}
}

func TestKubeletAuthNoAuthAllowsAnonymous(t *testing.T) {
	// No client and no CA file: empty ClientCA must not open a CA path.
	h := applyKubeletAuth(t, "node-a", "", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stats/summary", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("anonymous status %d body %q", rr.Code, rr.Body.String())
	}
}

func TestKubeletAuthWhitespaceCAIsNoAuth(t *testing.T) {
	h := applyKubeletAuth(t, "node-a", "  ", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/logs", nil))
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status %d body %q", rr.Code, rr.Body.String())
	}
}

func TestKubeletAuthRequiresHandler(t *testing.T) {
	nc := &nodeutil.NodeConfig{}
	if err := kubeletAuth("node-a", "")(nc); err == nil || !strings.Contains(err.Error(), "handler") {
		t.Fatalf("nil handler: %v", err)
	}
}

func TestKubeletAuthWebhookBuildsAndRejectsAnonymous(t *testing.T) {
	ca := writeTempCA(t)
	h := applyKubeletAuth(t, "node-a", ca, fake.NewSimpleClientset())
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/stats/summary", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status %d body %q", rr.Code, rr.Body.String())
	}
}

func TestKubeletAuthWebhookRequiresClient(t *testing.T) {
	ca := writeTempCA(t)
	nc := &nodeutil.NodeConfig{Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})}
	err := kubeletAuth("node-a", ca)(nc)
	if err == nil || !strings.Contains(err.Error(), "kubernetes client") {
		t.Fatalf("got %v", err)
	}
}

func TestKubeletAuthWebhookRejectsMissingCA(t *testing.T) {
	nc := &nodeutil.NodeConfig{
		Client:  fake.NewSimpleClientset(),
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	}
	err := kubeletAuth("node-a", filepath.Join(t.TempDir(), "missing.pem"))(nc)
	if err == nil {
		t.Fatal("missing CA must fail closed")
	}
}

func TestHelmClusterRoleAllowsAuthReviews(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller")
	}
	path := filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "darwin-node", "templates", "rbac.yaml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, snippet := range []string{
		"apiGroups: [\"authentication.k8s.io\"]\n    resources: [\"tokenreviews\"]\n    verbs: [\"create\"]",
		"apiGroups: [\"authorization.k8s.io\"]\n    resources: [\"subjectaccessreviews\"]\n    verbs: [\"create\"]",
	} {
		if !strings.Contains(text, snippet) {
			t.Fatalf("helm ClusterRole missing webhook auth rule:\n%s", snippet)
		}
	}
}

func TestKubeletAuthWebhookRejectsBadCA(t *testing.T) {
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("not a certificate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nc := &nodeutil.NodeConfig{
		Client:  fake.NewSimpleClientset(),
		Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
	}
	if err := kubeletAuth("node-a", bad)(nc); err == nil {
		t.Fatal("invalid CA must fail closed")
	}
}

func applyKubeletAuth(t *testing.T, nodeName, clientCA string, client kubernetes.Interface) http.Handler {
	t.Helper()
	nc := &nodeutil.NodeConfig{
		Client: client,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	if err := kubeletAuth(nodeName, clientCA)(nc); err != nil {
		t.Fatal(err)
	}
	return nc.Handler
}

func writeTempCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "client-ca", Organization: []string{"darwin-node-test"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "client-ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
