package image

import (
	"encoding/json"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
)

type dockerCfg struct {
	Auths map[string]dockerAuth `json:"auths"`
}

type dockerAuth struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	IdentityToken string `json:"identitytoken"`
	Auth          string `json:"auth"`
}

// RegistryCreds is the credential used for one image pull.
type RegistryCreds struct {
	Server        string
	Username      string
	Password      string
	IdentityToken string
}

func (c RegistryCreds) Empty() bool {
	return c.Username == "" && c.Password == "" && c.IdentityToken == ""
}

func (c RegistryCreds) String() string {
	if c.Empty() {
		return "RegistryCreds{anonymous}"
	}
	return fmt.Sprintf("RegistryCreds{server:%s user:%s password:redacted token:%t}", c.Server, c.Username, c.IdentityToken != "")
}

func (c RegistryCreds) GoString() string { return c.String() }

// ParsePullSecret extracts auths from a kubernetes.io/dockerconfigjson or dockercfg secret.
func ParsePullSecret(sec *corev1.Secret) (map[string]RegistryCreds, error) {
	if sec == nil {
		return nil, fmt.Errorf("nil secret")
	}
	var raw []byte
	switch sec.Type {
	case corev1.SecretTypeDockerConfigJson:
		raw = sec.Data[corev1.DockerConfigJsonKey]
	case corev1.SecretTypeDockercfg:
		if v, ok := sec.Data[corev1.DockerConfigJsonKey]; ok {
			raw = v
		} else {
			raw = sec.Data[corev1.DockerConfigKey]
		}
	default:
		return nil, fmt.Errorf("unsupported secret type %s", sec.Type)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("empty docker config in secret %s", sec.Name)
	}
	var cfg dockerCfg
	if err := json.Unmarshal(raw, &cfg); err != nil {
		var auths map[string]dockerAuth
		if err2 := json.Unmarshal(raw, &auths); err2 != nil {
			return nil, fmt.Errorf("parse docker config: %w", err)
		}
		cfg.Auths = auths
	}
	if len(cfg.Auths) == 0 {
		return nil, fmt.Errorf("empty auths in secret %s", sec.Name)
	}
	out := map[string]RegistryCreds{}
	regs := make([]string, 0, len(cfg.Auths))
	for r := range cfg.Auths {
		regs = append(regs, r)
	}
	sort.Strings(regs)
	for _, r := range regs {
		a := cfg.Auths[r]
		out[r] = RegistryCreds{
			Server:        r,
			Username:      a.Username,
			Password:      a.Password,
			IdentityToken: a.IdentityToken,
		}
	}
	return out, nil
}
