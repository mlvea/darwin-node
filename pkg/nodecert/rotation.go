package nodecert

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"time"

	certificatesv1 "k8s.io/api/certificates/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/certificate"
)

// Rotation is a client-go certificate.Manager plus the store it writes.
// Current hides an expired certificate. Retained still returns it, so a
// handshake or apiserver call during an outage keeps the last pair
// instead of dropping it.
type Rotation struct {
	mgr   certificate.Manager
	store certificate.Store
}

// ServingOptions starts a kubelet serving certificate manager.
// ClientsetFn must talk to the API as the node (or as the bootstrap
// identity). The serving certificate is not a client credential, so the
// function should ignore the certificate it is passed.
type ServingOptions struct {
	Context     context.Context
	NodeName    string
	CertDir     string
	Addresses   []corev1.NodeAddress
	CertFile    string
	KeyFile     string
	ClientsetFn certificate.ClientsetFunc
}

// ClientOptions starts a node client certificate manager.
type ClientOptions struct {
	Context     context.Context
	NodeName    string
	CertDir     string
	CertFile    string
	KeyFile     string
	ClientsetFn certificate.ClientsetFunc
}

// StartServing starts rotation for signer kubernetes.io/kubelet-serving.
// The process does not approve the CSR.
func StartServing(opts ServingOptions) (*Rotation, error) {
	if opts.ClientsetFn == nil {
		return nil, errors.New("serving certificate client is required")
	}
	nodeName := opts.NodeName
	addrs := append([]corev1.NodeAddress(nil), opts.Addresses...)
	return start(opts.Context, opts.NodeName, opts.CertDir, ServerPairPrefix, opts.CertFile, opts.KeyFile, certificatesv1.KubeletServingSignerName, certificate.DefaultKubeletServingGetUsages, func() *x509.CertificateRequest {
		return ServerTemplate(nodeName, addrs)
	}, opts.ClientsetFn)
}

// StartClient starts rotation for signer
// kubernetes.io/kube-apiserver-client-kubelet.
func StartClient(opts ClientOptions) (*Rotation, error) {
	if opts.ClientsetFn == nil {
		return nil, errors.New("client certificate client is required")
	}
	nodeName := opts.NodeName
	return start(opts.Context, opts.NodeName, opts.CertDir, ClientPairPrefix, opts.CertFile, opts.KeyFile, certificatesv1.KubeAPIServerClientKubeletSignerName, certificate.DefaultKubeletClientGetUsages, func() *x509.CertificateRequest {
		return ClientTemplate(nodeName)
	}, opts.ClientsetFn)
}

func start(ctx context.Context, nodeName, dir, prefix, certFile, keyFile, signer string, usages func(privateKey interface{}) []certificatesv1.KeyUsage, template func() *x509.CertificateRequest, clientsetFn certificate.ClientsetFunc) (*Rotation, error) {
	if nodeName == "" {
		return nil, errors.New("node name is required")
	}
	store, err := newSecureStore(dir, prefix, certFile, keyFile)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	mgr, err := certificate.NewManager(&certificate.Config{
		ClientsetFn:      clientsetFn,
		GetTemplate:      template,
		SignerName:       signer,
		GetUsages:        usages,
		CertificateStore: store,
		Name:             signer,
		Ctx:              &ctx,
	})
	if err != nil {
		return nil, err
	}
	mgr.Start()
	return &Rotation{mgr: mgr, store: store}, nil
}

// Stop ends the manager's background work. It is safe to call more than once.
func (r *Rotation) Stop() {
	if r == nil || r.mgr == nil {
		return
	}
	r.mgr.Stop()
}

// Current returns the manager's certificate. It is nil when none has been
// issued or when the issued certificate is past NotAfter.
func (r *Rotation) Current() *tls.Certificate {
	if r == nil || r.mgr == nil {
		return nil
	}
	return r.mgr.Current()
}

// Retained returns the certificate last written to the store, including
// one the manager no longer returns because it has expired.
func (r *Rotation) Retained() (*tls.Certificate, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("no certificate store")
	}
	return r.store.Current()
}

// Live is the certificate to present on a new connection: the manager's
// current certificate, or the retained one when that is hidden.
func (r *Rotation) Live() (*tls.Certificate, error) {
	if c := r.Current(); c != nil {
		return c, nil
	}
	return r.Retained()
}

// ServerHealthy reports whether the manager's last certificate request
// reached the API server.
func (r *Rotation) ServerHealthy() bool {
	if r == nil || r.mgr == nil {
		return false
	}
	return r.mgr.ServerHealthy()
}

// Wait blocks until Current returns a certificate or ctx ends.
func (r *Rotation) Wait(ctx context.Context) error {
	if r == nil {
		return errors.New("no certificate rotation")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if c := r.Current(); c != nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for certificate: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

// EncodePEM returns the certificate chain and private key in PEM form.
func EncodePEM(c *tls.Certificate) (certPEM, keyPEM []byte, err error) {
	if c == nil || len(c.Certificate) == 0 || c.PrivateKey == nil {
		return nil, nil, errors.New("incomplete certificate")
	}
	for _, der := range c.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(c.PrivateKey)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal private key: %w", err)
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}
