package image

import (
	"encoding/base64"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParsePullSecret(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "reg"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"username":"u","password":"p"}}}`),
		},
	}
	m, err := ParsePullSecret(sec)
	if err != nil {
		t.Fatal(err)
	}
	c := m["ghcr.io"]
	if c.Username != "u" || c.Password != "p" {
		t.Fatalf("%+v", c)
	}
	if !strings.Contains(c.String(), "redacted") {
		t.Fatalf("redaction: %s", c.String())
	}
}

func TestParsePullSecretRejectsOtherTypes(t *testing.T) {
	_, err := ParsePullSecret(&corev1.Secret{Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"x": []byte("y")}})
	if err == nil {
		t.Fatal("expected error")
	}
}

// Docker writes "auth" as base64(username:password) and often omits the
// username and password fields. Ignoring auth makes the pull anonymous.
func TestParsePullSecretDecodesAuthField(t *testing.T) {
	auth := base64.StdEncoding.EncodeToString([]byte("user:s3cret"))
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "reg"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"auth":"` + auth + `"}}}`),
		},
	}
	m, err := ParsePullSecret(sec)
	if err != nil {
		t.Fatal(err)
	}
	c := m["ghcr.io"]
	if c.Username != "user" || c.Password != "s3cret" {
		t.Fatalf("auth field was not decoded: %+v", c)
	}
}

func TestPullCredsMatchRegistryHost(t *testing.T) {
	hub := base64.StdEncoding.EncodeToString([]byte("hub:hubpass"))
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "reg"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{
				"https://index.docker.io/v1/":{"auth":"` + hub + `"},
				"ghcr.io":{"username":"real","password":"secret"},
				"ghcr.io/org":{"username":"org","password":"orgpass"},
				"*.example.com":{"username":"star","password":"starpass"},
				"127.0.0.1:5000":{"username":"local","password":"localpass"},
				"aaa.example":{"username":"attacker","password":"stolen"}
			}}`),
		},
	}
	parsed, err := ParsePullSecret(sec)
	if err != nil {
		t.Fatal(err)
	}
	creds, err := NewPullCreds(CredList(parsed))
	if err != nil {
		t.Fatal(err)
	}
	checks := []struct {
		ref  string
		user string
	}{
		{"team/macos:15", "hub"},
		{"docker.io/library/macos:15", "hub"},
		{"nginx", "hub"},
		{"ghcr.io/org/macos:15", "org"},
		{"ghcr.io/other/macos:15", "real"},
		{"reg.example.com/macos:15", "star"},
		{"example.com/macos:15", ""},
		{"127.0.0.1:5000/darwin/macos:v1", "local"},
		{"127.0.0.1:5001/darwin/macos:v1", ""},
		{"quay.io/team/macos:15", ""},
		{"aaa.example/team/macos:15", "attacker"},
	}
	for _, c := range checks {
		got := creds.For(c.ref)
		if got.Username != c.user {
			t.Errorf("%s: user %q, want %q", c.ref, got.Username, c.user)
		}
		if c.user == "" && !got.Empty() {
			t.Errorf("%s: unrelated credential %+v", c.ref, got)
		}
	}
}

func TestParsePullSecretRejectsUndecodableAuth(t *testing.T) {
	sec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "reg"},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: []byte(`{"auths":{"ghcr.io":{"auth":"@@@"}}}`),
		},
	}
	if _, err := ParsePullSecret(sec); err == nil {
		t.Fatal("undecodable auth field was accepted")
	}
}
