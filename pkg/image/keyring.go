package image

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/distribution/reference"
)

// pullKeyring selects a docker-config credential the way kubelet does.
// The host (and port) must match, a credential path must be a prefix of
// the image repository, and the longest key wins. /v1/ and /v2/ on a
// config key are the host itself. index.docker.io is Docker Hub for
// names with no registry. A key that does not match is not sent.
type pullKeyring struct {
	entries []keyedCred
}

type keyedCred struct {
	key   string
	order int
	cred  RegistryCreds
}

const defaultRegistryHost = "index.docker.io"

// NewPullCreds builds a credential that selects an entry per image.
// The returned value's own username and password are empty; call For.
func NewPullCreds(entries []RegistryCreds) (RegistryCreds, error) {
	k := &pullKeyring{}
	for _, e := range entries {
		if err := k.add(e); err != nil {
			return RegistryCreds{}, err
		}
	}
	return RegistryCreds{keyring: k}, nil
}

func (k *pullKeyring) add(c RegistryCreds) error {
	key, ok := dockerConfigKey(c.Server)
	if !ok {
		return fmt.Errorf("invalid registry %q", c.Server)
	}
	// For must return a single credential, not the whole ring.
	c.keyring = nil
	k.entries = append(k.entries, keyedCred{key: key, order: len(k.entries), cred: c})
	return nil
}

// For returns the credential to send when pulling ref.
// An explicit credential (no pull secrets) is returned unchanged.
// No matching secret entry is anonymous, not some other registry's secret.
func (c RegistryCreds) For(ref string) RegistryCreds {
	if c.keyring == nil {
		return c
	}
	got, ok := c.keyring.lookup(ref)
	if !ok {
		return RegistryCreds{}
	}
	return got
}

func (k *pullKeyring) lookup(ref string) (RegistryCreds, bool) {
	if k == nil {
		return RegistryCreds{}, false
	}
	names := imageLookupNames(ref)
	var best *keyedCred
	for _, name := range names {
		for i := range k.entries {
			e := &k.entries[i]
			if !urlsMatch(e.key, name) {
				continue
			}
			if best == nil || len(e.key) > len(best.key) || (len(e.key) == len(best.key) && e.order < best.order) {
				best = e
			}
		}
	}
	if best != nil {
		return best.cred, true
	}
	for _, name := range names {
		if !isDefaultRegistryMatch(name) {
			continue
		}
		for i := range k.entries {
			if k.entries[i].key == defaultRegistryHost {
				return k.entries[i].cred, true
			}
		}
	}
	return RegistryCreds{}, false
}

// dockerConfigKey normalizes a dockercfg registry key to host[:port][/path].
// https://index.docker.io/v1/ and https://quay.io/v2/ are the host alone.
func dockerConfigKey(loc string) (string, bool) {
	value := strings.TrimSpace(loc)
	if value == "" {
		return "", false
	}
	if !strings.HasPrefix(value, "https://") && !strings.HasPrefix(value, "http://") {
		value = "https://" + value
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" {
		return "", false
	}
	effectivePath := parsed.Path
	if strings.HasPrefix(effectivePath, "/v2/") || strings.HasPrefix(effectivePath, "/v1/") {
		effectivePath = effectivePath[3:]
	}
	if len(effectivePath) > 0 && effectivePath != "/" {
		return parsed.Host + effectivePath, true
	}
	return parsed.Host, true
}

func imageLookupNames(ref string) []string {
	ref = strings.TrimSpace(ref)
	named, err := reference.ParseNormalizedNamed(ref)
	if err != nil {
		if i := strings.LastIndex(ref, "@"); i > 0 {
			ref = ref[:i]
		}
		return []string{ref}
	}
	name := named.Name()
	familiar := reference.FamiliarName(named)
	if familiar == name {
		return []string{name}
	}
	return []string{name, familiar}
}

// urlsMatch is kubelet's URLsMatch: same port, same number of host labels,
// filepath.Match per label, credential path is a prefix of the image path.
func urlsMatch(glob, target string) bool {
	g, err := url.Parse("https://" + glob)
	if err != nil || g.Host == "" {
		return false
	}
	t, err := url.Parse("https://" + target)
	if err != nil || t.Host == "" {
		return false
	}
	gHost, gPort := splitHostPort(g.Host)
	tHost, tPort := splitHostPort(t.Host)
	if gPort != tPort {
		return false
	}
	gParts := strings.Split(gHost, ".")
	tParts := strings.Split(tHost, ".")
	if len(gParts) != len(tParts) {
		return false
	}
	if !strings.HasPrefix(t.Path, g.Path) {
		return false
	}
	for i := range gParts {
		ok, err := filepath.Match(gParts[i], tParts[i])
		if err != nil || !ok {
			return false
		}
	}
	return true
}

func splitHostPort(hostport string) (host, port string) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, ""
	}
	return host, port
}

// isDefaultRegistryMatch reports Docker Hub names: no registry, docker.io,
// or index.docker.io. A host with '.' or ':' is some other registry.
func isDefaultRegistryMatch(image string) bool {
	parts := strings.SplitN(image, "/", 2)
	if len(parts[0]) == 0 {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	if parts[0] == "docker.io" || parts[0] == defaultRegistryHost {
		return true
	}
	return !strings.ContainsAny(parts[0], ".:")
}
