package image

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
// A value from NewPullCreds holds every entry from the pod's pull
// secrets. For selects the entry for one image. A value built by the
// caller (the image CLI, a test) is sent as-is.
type RegistryCreds struct {
	Server        string
	Username      string
	Password      string
	IdentityToken string

	keyring *pullKeyring
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
		user, pass := a.Username, a.Password
		// Docker's canonical field is auth: base64(username:password).
		// Kubelet decodes it over the username and password fields.
		// Leaving it unread made these secrets an anonymous pull.
		if a.Auth != "" {
			var err error
			user, pass, err = decodeDockerAuth(a.Auth)
			if err != nil {
				return nil, fmt.Errorf("secret %s registry %s: %w", sec.Name, r, err)
			}
		}
		out[r] = RegistryCreds{
			Server:        r,
			Username:      user,
			Password:      pass,
			IdentityToken: a.IdentityToken,
		}
	}
	return out, nil
}

// decodeDockerAuth reads the dockercfg auth field. Padded and unpadded
// base64 both occur. The payload is username:password, split on the first colon.
func decodeDockerAuth(field string) (username, password string, err error) {
	field = strings.TrimSpace(field)
	var decoded []byte
	if strings.HasSuffix(field, "=") {
		decoded, err = base64.StdEncoding.DecodeString(field)
	} else {
		decoded, err = base64.RawStdEncoding.DecodeString(field)
	}
	if err != nil {
		return "", "", fmt.Errorf("auth field: %w", err)
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", fmt.Errorf("auth field must be base64(username:password)")
	}
	return parts[0], parts[1], nil
}

// CredList returns secret entries in registry-key order so two resolves
// of the same secret select the same credential when specificity ties.
func CredList(m map[string]RegistryCreds) []RegistryCreds {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]RegistryCreds, 0, len(keys))
	for _, k := range keys {
		out = append(out, m[k])
	}
	return out
}
