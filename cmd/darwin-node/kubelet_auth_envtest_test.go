//go:build integration

package main

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
	"io"
	"log"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/darwin-node/darwin-node/pkg/config"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// envCfg is set by TestMain when kube-apiserver and etcd are available.
var envCfg *rest.Config

func TestMain(m *testing.M) {
	os.Exit(runEnvtest(m))
}

func runEnvtest(m *testing.M) (code int) {
	download, assets, fatal := envtestMode()
	if fatal != "" {
		fmt.Fprintln(os.Stderr, fatal)
		return 1
	}
	if !download && assets == "" {
		return m.Run()
	}
	env := &envtest.Environment{
		BinaryAssetsDirectory:       assets,
		DownloadBinaryAssets:        download,
		DownloadBinaryAssetsVersion: os.Getenv("DARWIN_NODE_ENVTEST_VERSION"),
		ControlPlaneStartTimeout:    90 * time.Second,
	}
	cfg, err := env.Start()
	if err != nil {
		fmt.Fprintf(os.Stderr, "envtest start: %v\n", err)
		return 1
	}
	defer func() {
		if err := env.Stop(); err != nil {
			fmt.Fprintf(os.Stderr, "envtest stop: %v\n", err)
			if code == 0 {
				code = 1
			}
		}
	}()
	envCfg = cfg
	return m.Run()
}

// envtestMode decides whether this process should start kube-apiserver.
// Default `go test -tags=integration` with no assets skips the live cases.
// DARWIN_NODE_REQUIRE_ENVTEST=1 turns a missing setup into a failure so CI
// cannot pass by skipping.
func envtestMode() (download bool, assets, fatal string) {
	require := os.Getenv("DARWIN_NODE_REQUIRE_ENVTEST") == "1"
	assets = os.Getenv("KUBEBUILDER_ASSETS")
	if assets != "" {
		if err := checkEnvtestAssets(assets); err != nil {
			if require {
				return false, "", err.Error()
			}
			return false, "", ""
		}
		return false, assets, ""
	}
	if os.Getenv("DARWIN_NODE_ENVTEST_DOWNLOAD") == "1" {
		return true, os.Getenv("DARWIN_NODE_ENVTEST_ASSET_DIR"), ""
	}
	if require {
		return false, "", "KUBEBUILDER_ASSETS is unset and DARWIN_NODE_ENVTEST_DOWNLOAD is not 1"
	}
	return false, "", ""
}

func checkEnvtestAssets(dir string) error {
	for _, name := range []string{"kube-apiserver", "etcd"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			return fmt.Errorf("envtest binary %s: %w", name, err)
		}
	}
	return nil
}

func TestKubeletAuthEnvtest(t *testing.T) {
	t.Run("missing CA fails closed", func(t *testing.T) {
		nc := &nodeutil.NodeConfig{
			Client:  fake.NewSimpleClientset(),
			Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		}
		err := kubeletAuth("envtest-node", filepath.Join(t.TempDir(), "missing.pem"))(nc)
		if err == nil {
			t.Fatal("missing CA must fail closed")
		}
	})
	t.Run("invalid CA fails closed", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.pem")
		if err := os.WriteFile(bad, []byte("not a certificate\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		nc := &nodeutil.NodeConfig{
			Client:  fake.NewSimpleClientset(),
			Handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}),
		}
		if err := kubeletAuth("envtest-node", bad)(nc); err == nil {
			t.Fatal("invalid CA must fail closed")
		}
	})

	if envCfg == nil {
		t.Skip("envtest binaries not available (set KUBEBUILDER_ASSETS or DARWIN_NODE_ENVTEST_DOWNLOAD=1)")
	}

	const (
		nodeName   = "envtest-node"
		ns         = "kubelet-auth"
		logUser    = "mtls-log-client"
		otherUser  = "mtls-other-node"
		proxySA    = "proxy-reader"
		noAccessSA = "no-access"
		kubeletSA  = "kubelet"
	)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	admin, err := kubernetes.NewForConfig(envCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := admin.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: ns},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	kubeletToken := createSAToken(ctx, t, admin, ns, kubeletSA)
	proxyToken := createSAToken(ctx, t, admin, ns, proxySA)
	noAccessToken := createSAToken(ctx, t, admin, ns, noAccessSA)

	mustRole(ctx, t, admin, "envtest-kubelet-webhook", []rbacv1.PolicyRule{
		{APIGroups: []string{"authentication.k8s.io"}, Resources: []string{"tokenreviews"}, Verbs: []string{"create"}},
		{APIGroups: []string{"authorization.k8s.io"}, Resources: []string{"subjectaccessreviews"}, Verbs: []string{"create"}},
	})
	mustBind(ctx, t, admin, "envtest-kubelet-webhook", "envtest-kubelet-webhook", []rbacv1.Subject{{
		Kind: "ServiceAccount", Name: kubeletSA, Namespace: ns,
	}})
	mustRole(ctx, t, admin, "envtest-node-log", []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"nodes/log"}, ResourceNames: []string{nodeName}, Verbs: []string{"get"},
	}})
	mustBind(ctx, t, admin, "envtest-node-log", "envtest-node-log", []rbacv1.Subject{{
		APIGroup: "rbac.authorization.k8s.io", Kind: "User", Name: logUser,
	}})
	mustRole(ctx, t, admin, "envtest-node-log-other", []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"nodes/log"}, ResourceNames: []string{"other-node"}, Verbs: []string{"get"},
	}})
	mustBind(ctx, t, admin, "envtest-node-log-other", "envtest-node-log-other", []rbacv1.Subject{{
		APIGroup: "rbac.authorization.k8s.io", Kind: "User", Name: otherUser,
	}})
	mustRole(ctx, t, admin, "envtest-node-proxy", []rbacv1.PolicyRule{{
		APIGroups: []string{""}, Resources: []string{"nodes/proxy"}, ResourceNames: []string{nodeName}, Verbs: []string{"get"},
	}})
	mustBind(ctx, t, admin, "envtest-node-proxy", "envtest-node-proxy", []rbacv1.Subject{{
		Kind: "ServiceAccount", Name: proxySA, Namespace: ns,
	}})

	kubeClient, err := clientWithToken(envCfg, kubeletToken)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kubeClient.RbacV1().ClusterRoles().List(ctx, metav1.ListOptions{}); err == nil || !apierrors.IsForbidden(err) {
		t.Fatalf("kubelet client must be limited to auth reviews, list clusterroles: %v", err)
	}

	proxyUser := "system:serviceaccount:" + ns + ":" + proxySA
	waitTokenReview(ctx, t, kubeClient, proxyToken, proxyUser)
	waitSAR(ctx, t, kubeClient, logUser, "", "log", nodeName, true)
	waitSAR(ctx, t, kubeClient, proxyUser, "", "proxy", nodeName, true)
	waitSAR(ctx, t, kubeClient, logUser, "", "proxy", nodeName, false)
	waitSAR(ctx, t, kubeClient, "system:serviceaccount:"+ns+":"+noAccessSA, "", "log", nodeName, false)

	dir := t.TempDir()
	serverCA, serverCAKey := makeCA(t, "server-ca")
	serverKey, serverDER := makeLeaf(t, "localhost", serverCA, serverCAKey, false, true)
	clientCA, clientCAKey := makeCA(t, "client-ca")
	logKey, logDER := makeLeaf(t, logUser, clientCA, clientCAKey, true, false)
	otherKey, otherDER := makeLeaf(t, otherUser, clientCA, clientCAKey, true, false)
	intruderCA, intruderCAKey := makeCA(t, "intruder-ca")
	intruderKey, intruderDER := makeLeaf(t, "intruder", intruderCA, intruderCAKey, true, false)

	serverCert := writePEM(t, dir, "server.crt", "CERTIFICATE", serverDER)
	serverKeyPath := writeKey(t, dir, "server.key", serverKey)
	clientCAPath := writePEM(t, dir, "client-ca.crt", "CERTIFICATE", clientCA.Raw)

	tlsCfg, err := config.TLSConfig(serverCert, serverKeyPath, clientCAPath)
	if err != nil {
		t.Fatal(err)
	}
	if tlsCfg.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Fatalf("ClientAuth %v", tlsCfg.ClientAuth)
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	nc := &nodeutil.NodeConfig{Client: kubeClient, Handler: inner}
	if err := kubeletAuth(nodeName, clientCAPath)(nc); err != nil {
		t.Fatal(err)
	}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", tlsCfg)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{
		Handler:           nc.Handler,
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	roots := x509.NewCertPool()
	roots.AddCert(serverCA)
	base := "https://" + ln.Addr().String()

	logClient := httpClient(roots, &tls.Certificate{Certificate: [][]byte{logDER}, PrivateKey: logKey})
	otherClient := httpClient(roots, &tls.Certificate{Certificate: [][]byte{otherDER}, PrivateKey: otherKey})
	anonClient := httpClient(roots, nil)
	intruderClient := httpClientForceCert(roots, &tls.Certificate{Certificate: [][]byte{intruderDER}, PrivateKey: intruderKey})

	t.Run("client cert signed by ClientCA is authenticated", func(t *testing.T) {
		code, body := getStatus(t, logClient, base+"/logs/", "")
		if code != http.StatusNoContent {
			t.Fatalf("status %d body %q", code, body)
		}
	})
	t.Run("client cert denied for nodes/proxy", func(t *testing.T) {
		code, body := getStatus(t, logClient, base+"/exec", "")
		if code != http.StatusForbidden {
			t.Fatalf("status %d body %q", code, body)
		}
	})
	t.Run("client cert for a different node name is denied", func(t *testing.T) {
		code, body := getStatus(t, otherClient, base+"/logs/", "")
		if code != http.StatusForbidden {
			t.Fatalf("status %d body %q", code, body)
		}
	})
	t.Run("client cert signed by another CA is rejected", func(t *testing.T) {
		req, err := http.NewRequest(http.MethodGet, base+"/logs/", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := intruderClient.Do(req); err == nil {
			t.Fatal("untrusted client certificate completed TLS")
		}
	})
	t.Run("serviceaccount bearer token is authenticated via TokenReview", func(t *testing.T) {
		code, body := getStatus(t, anonClient, base+"/exec", proxyToken)
		if code != http.StatusNoContent {
			t.Fatalf("status %d body %q", code, body)
		}
	})
	t.Run("serviceaccount proxy role is denied for nodes/log", func(t *testing.T) {
		code, body := getStatus(t, anonClient, base+"/logs/", proxyToken)
		if code != http.StatusForbidden {
			t.Fatalf("status %d body %q", code, body)
		}
	})
	t.Run("serviceaccount without a role is denied", func(t *testing.T) {
		code, body := getStatus(t, anonClient, base+"/logs/", noAccessToken)
		if code != http.StatusForbidden {
			t.Fatalf("status %d body %q", code, body)
		}
	})
	t.Run("invalid bearer token is unauthorized", func(t *testing.T) {
		code, body := getStatus(t, anonClient, base+"/logs/", "not-a-real-token")
		if code != http.StatusUnauthorized {
			t.Fatalf("status %d body %q", code, body)
		}
	})
	t.Run("anonymous request is unauthorized", func(t *testing.T) {
		code, body := getStatus(t, anonClient, base+"/logs/", "")
		if code != http.StatusUnauthorized {
			t.Fatalf("status %d body %q", code, body)
		}
	})
}

func createSAToken(ctx context.Context, t *testing.T, c kubernetes.Interface, ns, name string) string {
	t.Helper()
	if _, err := c.CoreV1().ServiceAccounts(ns).Create(ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	var last error
	for i := 0; i < 20; i++ {
		tr, err := c.CoreV1().ServiceAccounts(ns).CreateToken(ctx, name, &authenticationv1.TokenRequest{}, metav1.CreateOptions{})
		if err == nil && tr.Status.Token != "" {
			return tr.Status.Token
		}
		last = err
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("token for %s/%s: %v", ns, name, last)
	return ""
}

func mustRole(ctx context.Context, t *testing.T, c kubernetes.Interface, name string, rules []rbacv1.PolicyRule) {
	t.Helper()
	_, err := c.RbacV1().ClusterRoles().Create(ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Rules:      rules,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func mustBind(ctx context.Context, t *testing.T, c kubernetes.Interface, name, role string, subs []rbacv1.Subject) {
	t.Helper()
	_, err := c.RbacV1().ClusterRoleBindings().Create(ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "ClusterRole",
			Name:     role,
		},
		Subjects: subs,
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
}

func clientWithToken(base *rest.Config, token string) (kubernetes.Interface, error) {
	cfg := rest.CopyConfig(base)
	cfg.CertData = nil
	cfg.KeyData = nil
	cfg.CertFile = ""
	cfg.KeyFile = ""
	cfg.BearerToken = token
	cfg.BearerTokenFile = ""
	cfg.Username = ""
	cfg.Password = ""
	cfg.Impersonate = rest.ImpersonationConfig{}
	cfg.AuthProvider = nil
	cfg.ExecProvider = nil
	return kubernetes.NewForConfig(cfg)
}

func waitTokenReview(ctx context.Context, t *testing.T, c kubernetes.Interface, token, wantUser string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		tr, err := c.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
			Spec: authenticationv1.TokenReviewSpec{Token: token},
		}, metav1.CreateOptions{})
		if err != nil {
			last = err.Error()
		} else if tr.Status.Authenticated && tr.Status.User.Username == wantUser {
			return
		} else {
			last = fmt.Sprintf("authenticated=%v user=%q err=%s", tr.Status.Authenticated, tr.Status.User.Username, tr.Status.Error)
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("TokenReview for %s: %s", wantUser, last)
}

func waitSAR(ctx context.Context, t *testing.T, c kubernetes.Interface, user, group, subresource, name string, want bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	var last string
	spec := authorizationv1.SubjectAccessReviewSpec{
		User: user,
		ResourceAttributes: &authorizationv1.ResourceAttributes{
			Verb: "get", Group: "", Version: "v1", Resource: "nodes", Subresource: subresource, Name: name,
		},
	}
	if group != "" {
		spec.Groups = []string{group}
	}
	for time.Now().Before(deadline) {
		sar, err := c.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
			Spec: spec,
		}, metav1.CreateOptions{})
		if err != nil {
			last = err.Error()
		} else if sar.Status.Allowed == want {
			return
		} else {
			last = fmt.Sprintf("allowed=%v reason=%s", sar.Status.Allowed, sar.Status.Reason)
		}
		if !want && sarAllowed(last) {
			// A deny that is still allowed is not a cache lag of the binding we just created.
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("SAR user=%s nodes/%s name=%s want allowed=%v: %s", user, subresource, name, want, last)
}

func sarAllowed(last string) bool {
	return strings.HasPrefix(last, "allowed=true")
}

func httpClient(roots *x509.CertPool, cert *tls.Certificate) *http.Client {
	cfg := &tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
	}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	return httpClientFrom(cfg)
}

// httpClientForceCert presents cert even when the server's advertised client
// CAs do not include its issuer. A normal client would omit the certificate.
func httpClientForceCert(roots *x509.CertPool, cert *tls.Certificate) *http.Client {
	return httpClientFrom(&tls.Config{
		RootCAs:    roots,
		ServerName: "localhost",
		MinVersion: tls.VersionTLS12,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return cert, nil
		},
	})
}

func httpClientFrom(cfg *tls.Config) *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: cfg},
	}
}

func getStatus(t *testing.T, c *http.Client, rawURL, bearer string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, rawURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func makeCA(t *testing.T, cn string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          mustSerial(t),
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
	return parsed, key
}

func makeLeaf(t *testing.T, cn string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey, client, server bool) (*ecdsa.PrivateKey, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: mustSerial(t),
		Subject:      pkix.Name{CommonName: cn, Organization: []string{"darwin-node-clients"}},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		DNSNames:     []string{"localhost"},
	}
	if client {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	}
	if server {
		tmpl.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, parentKey)
	if err != nil {
		t.Fatal(err)
	}
	return key, der
}

func mustSerial(t *testing.T) *big.Int {
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
