package nodecert

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// NodeClient is the apiserver client whose TLS certificate rotates in
// process. Transport swaps to the new certificate without closing
// connections that are still inside RoundTrip.
type NodeClient struct {
	Rotation  *Rotation
	Client    kubernetes.Interface
	Transport http.RoundTripper
	Config    *rest.Config
}

// StartNodeClient starts client-certificate rotation. bootstrap is the
// kubeconfig used while no client certificate exists yet (the initial
// CSR). After a certificate is issued, API calls from Client present
// that certificate and not the bootstrap token.
func StartNodeClient(ctx context.Context, nodeName, certDir string, bootstrap *rest.Config) (*NodeClient, error) {
	if bootstrap == nil {
		return nil, fmt.Errorf("bootstrap rest config is required")
	}
	boot := rest.CopyConfig(bootstrap)
	rot, err := StartClient(ClientOptions{
		Context:  ctx,
		NodeName: nodeName,
		CertDir:  certDir,
		ClientsetFn: func(current *tls.Certificate) (kubernetes.Interface, error) {
			return clientForCert(boot, current)
		},
	})
	if err != nil {
		return nil, err
	}
	anon := clusterTransportConfig(boot)
	rt, err := NewRotatingTransport(anon, rot.Live)
	if err != nil {
		rot.Stop()
		return nil, err
	}
	if anon.QPS == 0 {
		anon.QPS = 5
	}
	if anon.Burst == 0 {
		anon.Burst = 10
	}
	client, err := kubernetes.NewForConfigAndClient(anon, &http.Client{Transport: rt})
	if err != nil {
		rot.Stop()
		return nil, err
	}
	return &NodeClient{Rotation: rot, Client: client, Transport: rt, Config: anon}, nil
}

// clientForCert uses current when the manager has one, and the bootstrap
// credentials otherwise. An expired certificate makes Current return nil,
// so renewal falls back to the bootstrap kubeconfig instead of presenting
// a certificate the apiserver will reject.
func clientForCert(bootstrap *rest.Config, current *tls.Certificate) (kubernetes.Interface, error) {
	cfg := rest.CopyConfig(bootstrap)
	if current == nil {
		return kubernetes.NewForConfig(cfg)
	}
	cfg = clusterTransportConfig(cfg)
	certPEM, keyPEM, err := EncodePEM(current)
	if err != nil {
		return nil, err
	}
	cfg.CertData = certPEM
	cfg.KeyData = keyPEM
	return kubernetes.NewForConfig(cfg)
}

// clusterTransportConfig keeps the apiserver address and CA and drops
// user credentials. The rotating transport supplies the client certificate.
func clusterTransportConfig(in *rest.Config) *rest.Config {
	out := rest.CopyConfig(in)
	out.Username = ""
	out.Password = ""
	out.BearerToken = ""
	out.BearerTokenFile = ""
	out.CertFile = ""
	out.KeyFile = ""
	out.CertData = nil
	out.KeyData = nil
	out.AuthProvider = nil
	out.ExecProvider = nil
	out.Impersonate = rest.ImpersonationConfig{}
	return out
}
