package main

import (
	"fmt"
	"strings"

	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
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

// kubeletAuth wraps nc.Handler with kubelet authn/authz.
//
// The handler must already be the provider mux (set before this opt runs).
// An empty clientCA installs nodeutil.NoAuth (anonymous after TLS).
// A non-empty clientCA installs nodeutil.WebhookAuth and loads that file
// as the client-certificate CA ("client-ca"). A missing or unreadable CA
// fails the opt; it does not fall back to NoAuth.
func kubeletAuth(nodeName, clientCA string) nodeutil.NodeOpt {
	clientCA = strings.TrimSpace(clientCA)
	if selectKubeletAuth(clientCA) == kubeletAuthNoAuth {
		return func(nc *nodeutil.NodeConfig) error {
			return installKubeletAuth(nc, nodeutil.NoAuth())
		}
	}
	return func(nc *nodeutil.NodeConfig) error {
		if nc.Client == nil {
			return fmt.Errorf("kubelet webhook auth requires a kubernetes client")
		}
		auth, err := nodeutil.WebhookAuth(nc.Client, nodeName, func(wcfg *nodeutil.WebhookAuthConfig) error {
			ca, err := dynamiccertificates.NewDynamicCAContentFromFile("client-ca", clientCA)
			if err != nil {
				return fmt.Errorf("client CA %q: %w", clientCA, err)
			}
			wcfg.AuthnConfig.ClientCertificateCAContentProvider = ca
			return nil
		})
		if err != nil {
			return fmt.Errorf("kubelet webhook auth: %w", err)
		}
		return installKubeletAuth(nc, auth)
	}
}

func installKubeletAuth(nc *nodeutil.NodeConfig, auth nodeutil.Auth) error {
	if nc.Handler == nil {
		return fmt.Errorf("kubelet auth: http handler is not set")
	}
	nc.Handler = nodeutil.WithAuth(auth, nc.Handler)
	return nil
}
