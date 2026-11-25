package engine

import "github.com/darwin-node/darwin-node/pkg/sidecar"

func (e *Engine) sidecarCreateOptions(creds Credentials) sidecar.CreateOptions {
	return sidecar.CreateOptions{
		AllowedHostPaths: append([]string(nil), e.cfg.AllowedHostPaths...),
		ConfigMaps:       creds.ConfigMaps,
		Secrets:          creds.Secrets,
		Pull:             creds.Pull,
	}
}
