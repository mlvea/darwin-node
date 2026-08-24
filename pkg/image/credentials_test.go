package image

import (
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
