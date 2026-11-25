package engine

import "github.com/mlvea/darwin-node/pkg/sidecar"

func (e *Engine) sidecarCreateOptions(creds Credentials) sidecar.CreateOptions {
	return sidecar.CreateOptions{
		AllowedHostPaths: append([]string(nil), e.cfg.AllowedHostPaths...),
		ConfigMaps:       creds.ConfigMaps,
		Secrets:          creds.Secrets,
		Pull:             creds.Pull,
	}
}
