package main

import (
	"context"
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"

	dnconfig "github.com/mlvea/darwin-node/pkg/config"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

type stubProvider struct{}

func (stubProvider) Current() *tls.Certificate { return nil }

func (stubProvider) Retained() (*tls.Certificate, error) { return nil, nil }

func TestClusterClientFlagOffDoesNotTouchCertDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	var gotPath string
	restore := swapCertHooks(t)
	defer restore()
	clientsetFromEnv = func(path string) (kubernetes.Interface, error) {
		gotPath = path
		return fake.NewSimpleClientset(), nil
	}
	connectNodeClient = func(context.Context, string, string, *rest.Config) (*nodeSession, error) {
		t.Fatal("client rotation started with the flags off")
		return nil, nil
	}
	connectServing = func(context.Context, dnconfig.Config, string, kubernetes.Interface) (dnconfig.CertificateProvider, func(), error) {
		t.Fatal("serving rotation started with the flags off")
		return nil, nil, nil
	}
	cfg := dnconfig.Default()
	cfg.KubeConfig = "/var/lib/kubeconfig"
	cfg.CertDir = dir
	cs, serving, stop, err := clusterClient(context.Background(), cfg, "10.0.0.8")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if cs == nil || serving != nil {
		t.Fatalf("client %v serving %v", cs, serving)
	}
	if gotPath != cfg.KubeConfig {
		t.Fatalf("kubeconfig %q", gotPath)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cert dir created with rotation off: %v", err)
	}
}

func TestClusterClientRotatesClientWithoutEnvKubeconfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	boot := writeKubeconfig(t, t.TempDir())
	want := fake.NewSimpleClientset()
	var nodeCalls int
	restore := swapCertHooks(t)
	defer restore()
	clientsetFromEnv = func(string) (kubernetes.Interface, error) {
		t.Fatal("client rotation loaded the static kubeconfig")
		return nil, nil
	}
	connectNodeClient = func(_ context.Context, nodeName, certDir string, cfg *rest.Config) (*nodeSession, error) {
		nodeCalls++
		if nodeName != "mini" || certDir != dir {
			t.Fatalf("node %q dir %q", nodeName, certDir)
		}
		if cfg == nil || cfg.BearerToken != "bootstrap" {
			t.Fatalf("bootstrap config %+v", cfg)
		}
		return &nodeSession{
			Client: want,
			Stop:   func() {},
			Wait:   func(context.Context) error { return nil },
		}, nil
	}
	connectServing = func(context.Context, dnconfig.Config, string, kubernetes.Interface) (dnconfig.CertificateProvider, func(), error) {
		t.Fatal("serving rotation started")
		return nil, nil, nil
	}
	cfg := dnconfig.Config{
		NodeName:                 "mini",
		CertDir:                  dir,
		RotateClientCertificates: true,
		BootstrapKubeconfig:      boot,
		KubeConfig:               "/unused",
	}
	cs, serving, stop, err := clusterClient(context.Background(), cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if cs != want || serving != nil || nodeCalls != 1 {
		t.Fatalf("client match %v serving %v calls %d", cs == want, serving, nodeCalls)
	}
}

func TestClusterClientServerUsesNodeClient(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	boot := writeKubeconfig(t, t.TempDir())
	want := fake.NewSimpleClientset()
	var saw kubernetes.Interface
	var sawIP string
	restore := swapCertHooks(t)
	defer restore()
	clientsetFromEnv = func(string) (kubernetes.Interface, error) {
		t.Fatal("loaded the static kubeconfig")
		return nil, nil
	}
	connectNodeClient = func(context.Context, string, string, *rest.Config) (*nodeSession, error) {
		return &nodeSession{Client: want, Stop: func() {}, Wait: func(context.Context) error { return nil }}, nil
	}
	connectServing = func(_ context.Context, cfg dnconfig.Config, ip string, client kubernetes.Interface) (dnconfig.CertificateProvider, func(), error) {
		saw = client
		sawIP = ip
		if cfg.NodeName != "mini" {
			t.Fatalf("node %q", cfg.NodeName)
		}
		return stubProvider{}, func() {}, nil
	}
	cfg := dnconfig.Config{
		NodeName:                 "mini",
		CertDir:                  dir,
		RotateClientCertificates: true,
		RotateServerCertificates: true,
		BootstrapKubeconfig:      boot,
	}
	_, serving, stop, err := clusterClient(context.Background(), cfg, "10.9.8.7")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if serving == nil || saw != want || sawIP != "10.9.8.7" {
		t.Fatalf("serving %v ip %q", serving, sawIP)
	}
}

func TestClusterClientServerOnlyUsesKubeconfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	want := fake.NewSimpleClientset()
	var envPath string
	var servingCalls int
	restore := swapCertHooks(t)
	defer restore()
	clientsetFromEnv = func(path string) (kubernetes.Interface, error) {
		envPath = path
		return want, nil
	}
	connectNodeClient = func(context.Context, string, string, *rest.Config) (*nodeSession, error) {
		t.Fatal("client rotation started")
		return nil, nil
	}
	connectServing = func(context.Context, dnconfig.Config, string, kubernetes.Interface) (dnconfig.CertificateProvider, func(), error) {
		servingCalls++
		return stubProvider{}, func() {}, nil
	}
	cfg := dnconfig.Config{
		NodeName:                 "mini",
		KubeConfig:               "/var/lib/kube",
		CertDir:                  dir,
		RotateServerCertificates: true,
	}
	cs, _, stop, err := clusterClient(context.Background(), cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if cs != want || envPath != cfg.KubeConfig || servingCalls != 1 {
		t.Fatalf("path %q calls %d", envPath, servingCalls)
	}
}

func TestClusterClientRefusesWritableDirBeforeClients(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	restore := swapCertHooks(t)
	defer restore()
	clientsetFromEnv = func(string) (kubernetes.Interface, error) {
		t.Fatal("kubeconfig loaded")
		return nil, nil
	}
	connectNodeClient = func(context.Context, string, string, *rest.Config) (*nodeSession, error) {
		t.Fatal("client rotation started")
		return nil, nil
	}
	connectServing = func(context.Context, dnconfig.Config, string, kubernetes.Interface) (dnconfig.CertificateProvider, func(), error) {
		t.Fatal("serving rotation started")
		return nil, nil, nil
	}
	cfg := dnconfig.Config{CertDir: dir, RotateServerCertificates: true}
	_, _, stop, err := clusterClient(context.Background(), cfg, "")
	defer stop()
	if err == nil {
		t.Fatal("world-writable cert dir was accepted")
	}
	fi, stErr := os.Stat(dir)
	if stErr != nil {
		t.Fatal(stErr)
	}
	if fi.Mode().Perm() != 0o777 {
		t.Fatalf("mode changed to %o", fi.Mode().Perm())
	}
}

func TestClusterClientMissingBootstrap(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	restore := swapCertHooks(t)
	defer restore()
	connectNodeClient = func(context.Context, string, string, *rest.Config) (*nodeSession, error) {
		t.Fatal("client rotation started without a kubeconfig")
		return nil, nil
	}
	cfg := dnconfig.Config{
		CertDir:                  dir,
		RotateClientCertificates: true,
		BootstrapKubeconfig:      filepath.Join(t.TempDir(), "missing"),
	}
	_, _, stop, err := clusterClient(context.Background(), cfg, "")
	defer stop()
	if err == nil {
		t.Fatal("missing bootstrap kubeconfig")
	}
}

func TestClusterClientStopsNodeWhenServingFails(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	boot := writeKubeconfig(t, t.TempDir())
	stopped := false
	restore := swapCertHooks(t)
	defer restore()
	connectNodeClient = func(context.Context, string, string, *rest.Config) (*nodeSession, error) {
		return &nodeSession{Client: fake.NewSimpleClientset(), Stop: func() { stopped = true }, Wait: func(context.Context) error { return nil }}, nil
	}
	connectServing = func(context.Context, dnconfig.Config, string, kubernetes.Interface) (dnconfig.CertificateProvider, func(), error) {
		return nil, nil, os.ErrInvalid
	}
	cfg := dnconfig.Config{
		NodeName:                 "mini",
		CertDir:                  dir,
		RotateClientCertificates: true,
		RotateServerCertificates: true,
		BootstrapKubeconfig:      boot,
	}
	_, _, stop, err := clusterClient(context.Background(), cfg, "")
	defer stop()
	if err == nil {
		t.Fatal("serving failure")
	}
	if !stopped {
		t.Fatal("node client was left running")
	}
}

func TestRotationRejectsStandalone(t *testing.T) {
	if err := rotationWithStandalone(true, dnconfig.Config{RotateServerCertificates: true}); err == nil {
		t.Fatal("server rotation with standalone")
	}
	if err := rotationWithStandalone(true, dnconfig.Config{RotateClientCertificates: true}); err == nil {
		t.Fatal("client rotation with standalone")
	}
	if err := rotationWithStandalone(false, dnconfig.Config{RotateServerCertificates: true}); err != nil {
		t.Fatal(err)
	}
	if err := rotationWithStandalone(true, dnconfig.Config{}); err != nil {
		t.Fatal(err)
	}
}

func swapCertHooks(t *testing.T) func() {
	t.Helper()
	prevEnv := clientsetFromEnv
	prevNode := connectNodeClient
	prevSrv := connectServing
	return func() {
		clientsetFromEnv = prevEnv
		connectNodeClient = prevNode
		connectServing = prevSrv
	}
}

func writeKubeconfig(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "bootstrap.kubeconfig")
	body := []byte(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: c
contexts:
- context:
    cluster: c
    user: u
  name: c
current-context: c
users:
- name: u
  user:
    token: bootstrap
`)
	if err := os.WriteFile(p, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
