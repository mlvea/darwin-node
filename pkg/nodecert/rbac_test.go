package nodecert

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestCSRRBACExamples(t *testing.T) {
	root := repoRoot(t)
	static := filepath.Join(root, "deploy", "csr", "clusterrolebindings.yaml")
	bindings := decodeBindings(t, static)
	if len(bindings) != 2 {
		t.Fatalf("%d bindings", len(bindings))
	}
	got := map[string]string{}
	for _, b := range bindings {
		if b.RoleRef.Kind != "ClusterRole" {
			t.Fatalf("%s role kind %s", b.Name, b.RoleRef.Kind)
		}
		if len(b.Subjects) != 1 || b.Subjects[0].Kind != "Group" {
			t.Fatalf("%s subjects %+v", b.Name, b.Subjects)
		}
		got[b.RoleRef.Name] = b.Subjects[0].Name
	}
	if got["system:certificates.k8s.io:certificatesigningrequests:nodeclient"] != "system:bootstrappers" {
		t.Fatalf("nodeclient subject %q", got["system:certificates.k8s.io:certificatesigningrequests:nodeclient"])
	}
	if got["system:certificates.k8s.io:certificatesigningrequests:selfnodeclient"] != "system:nodes" {
		t.Fatalf("selfnodeclient subject %q", got["system:certificates.k8s.io:certificatesigningrequests:selfnodeclient"])
	}
	for _, path := range []string{
		static,
		filepath.Join(root, "deploy", "csr", "chart", "templates", "clusterrolebindings.yaml"),
		filepath.Join(root, "deploy", "csr", "chart", "values.yaml"),
	} {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := strings.ToLower(string(body))
		if strings.Contains(text, "approve") {
			t.Fatalf("%s grants csr approval", path)
		}
		if strings.Contains(text, "certificatesigningrequests/approval") {
			t.Fatalf("%s names the approval resource", path)
		}
	}
	chart, err := os.ReadFile(filepath.Join(root, "deploy", "csr", "chart", "templates", "clusterrolebindings.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{
		"system:certificates.k8s.io:certificatesigningrequests:nodeclient",
		"system:certificates.k8s.io:certificatesigningrequests:selfnodeclient",
		".Values.bootstrapGroup",
		".Values.nodeGroup",
	} {
		if !strings.Contains(string(chart), needle) {
			t.Fatalf("chart missing %s", needle)
		}
	}
	values, err := os.ReadFile(filepath.Join(root, "deploy", "csr", "chart", "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(values), "system:bootstrappers") || !strings.Contains(string(values), "system:nodes") {
		t.Fatalf("values %s", values)
	}
}

func decodeBindings(t *testing.T, path string) []rbacv1.ClusterRoleBinding {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(body), 4096)
	var out []rbacv1.ClusterRoleBinding
	for {
		var b rbacv1.ClusterRoleBinding
		err := dec.Decode(&b)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if b.Name == "" && b.RoleRef.Name == "" {
			continue
		}
		out = append(out, b)
	}
	return out
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}
