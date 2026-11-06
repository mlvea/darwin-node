package main

import (
	"crypto/tls"
	"fmt"
	"strings"

	dnconfig "github.com/darwin-node/darwin-node/pkg/config"
	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
)

// kubeletAuthMode is how the kubelet HTTP handler authenticates callers.
type kubeletAuthMode string

const (
	// kubeletAuthNoAuth is anonymous after TLS. Used only when no client CA
	// is configured. Matches virtual-kubelet's setAuth empty-CA path.
	kubeletAuthNoAuth kubeletAuthMode = "noauth"
	// kubeletAuthWebhook is TokenReview + SubjectAccessReview, plus client
	// certificate verification against the configured CA.
	kubeletAuthWebhook kubeletAuthMode = "webhook"
)

// selectKubeletAuth chooses webhook auth only when a client CA path is set.
// WebhookAuth without a client-certificate CA disables mTLS inside the
// authenticator; the API server's kubelet client would then be rejected.
// Whitespace-only is treated as unset, matching pkg/config.TLSConfig.
func selectKubeletAuth(clientCA string) kubeletAuthMode {
	if strings.TrimSpace(clientCA) == "" {
		return kubeletAuthNoAuth
	}
	return kubeletAuthWebhook
}

// loadClientCA returns nil when path is unset. A path that is set but
// missing or unparsable is an error and must not fall through to NoAuth.
// The caller starts bundle.Run so a later replacement is observed.
func loadClientCA(path string) (*dnconfig.ClientCABundle, error) {
	if selectKubeletAuth(path) != kubeletAuthWebhook {
		return nil, nil
	}
	return dnconfig.NewClientCABundle(path)
}

// kubeletAuth wraps nc.Handler with kubelet authn/authz.
//
// The handler must already be the provider mux (set before this opt runs).
// A nil bundle installs nodeutil.NoAuth (anonymous after TLS).
// A bundle installs nodeutil.WebhookAuth and uses that same live CA for
// client-certificate verification. A client certificate signed by the
// current bundle authenticates as its CommonName. A request with no client
// certificate is authenticated with a bearer token via TokenReview.
// The client on nc must be allowed to create tokenreviews and
// subjectaccessreviews.
func kubeletAuth(nodeName string, bundle *dnconfig.ClientCABundle) nodeutil.NodeOpt {
	if bundle == nil {
		return func(nc *nodeutil.NodeConfig) error {
			return installKubeletAuth(nc, nodeutil.NoAuth())
		}
	}
	return func(nc *nodeutil.NodeConfig) error {
		if nc.Client == nil {
			return fmt.Errorf("kubelet webhook auth requires a kubernetes client")
		}
		auth, err := nodeutil.WebhookAuth(nc.Client, nodeName, func(wcfg *nodeutil.WebhookAuthConfig) error {
			wcfg.AuthnConfig.ClientCertificateCAContentProvider = bundle.Content()
			return nil
		})
		if err != nil {
			return fmt.Errorf("kubelet webhook auth: %w", err)
		}
		return installKubeletAuth(nc, auth)
	}
}

// applyKubeletTLS copies the server certificate onto dst. A bundle
// installs the live client-CA hook. Without one, client certificates
// are not requested: virtual-kubelet's base config asks for them, and
// leaving that in place would advertise an empty CA list.
func applyKubeletTLS(dst, src *tls.Config, bundle *dnconfig.ClientCABundle) {
	dst.Certificates = src.Certificates
	if src.MinVersion != 0 {
		dst.MinVersion = src.MinVersion
	}
	if bundle != nil {
		bundle.InstallTLS(dst)
		return
	}
	dst.ClientCAs = nil
	dst.ClientAuth = tls.NoClientCert
	dst.GetConfigForClient = nil
}

func installKubeletAuth(nc *nodeutil.NodeConfig, auth nodeutil.Auth) error {
	if nc.Handler == nil {
		return fmt.Errorf("kubelet auth: http handler is not set")
	}
	nc.Handler = nodeutil.WithAuth(auth, nc.Handler)
	return nil
}
