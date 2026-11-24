package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"text/template"

	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

// chartCase is the Helm root object (.Values, .Release) used to render
// deploy/helm/darwin-node/templates/rbac.yaml with Go's text/template.
// The chart template stays within those actions so this test and helm agree.
type chartCase struct {
	Values  map[string]any
	Release map[string]string
}

type policyRule struct {
	APIGroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

type rbacObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Rules   []policyRule `json:"rules"`
	RoleRef struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"roleRef"`
	Subjects []struct {
		Kind      string `json:"kind"`
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"subjects"`
}

func TestChartDefaultIdentityIsNode(t *testing.T) {
	vals := loadChartValues(t)
	rbac, _ := vals["rbac"].(map[string]any)
	if rbac == nil || rbac["identity"] != "node" {
		t.Fatalf("default rbac.identity = %v, want node (system:nodes + Node authorizer)", rbac)
	}
}

func TestChartNodeIdentityGrantsNoPodOrSecretRules(t *testing.T) {
	objs := renderChart(t, loadChartValues(t))
	for _, obj := range objs {
		switch obj.Kind {
		case "ClusterRole", "Role", "ClusterRoleBinding", "RoleBinding":
			t.Fatalf("node identity rendered %s %s/%s; Secret, ConfigMap, and pod access must come from the Node authorizer", obj.Kind, obj.Metadata.Namespace, obj.Metadata.Name)
		}
		for _, rule := range obj.Rules {
			if sensitiveRule(rule) {
				t.Fatalf("%s grants %v %v", obj.Kind, rule.Resources, rule.Verbs)
			}
		}
	}
}

func TestChartServiceAccountRequiresNamespaces(t *testing.T) {
	vals := loadChartValues(t)
	rbac := map[string]any{"identity": "serviceaccount", "namespaces": []any{}}
	vals["rbac"] = rbac
	_, err := renderChartErr(vals)
	if err == nil || !strings.Contains(err.Error(), "rbac.namespaces") {
		t.Fatalf("empty namespaces: %v", err)
	}
}

func TestChartServiceAccountScopesSecretPodAndTokenRules(t *testing.T) {
	vals := loadChartValues(t)
	vals["rbac"] = map[string]any{
		"identity":   "serviceaccount",
		"namespaces": []any{"build", "ci"},
	}
	objs := renderChart(t, vals)

	var clusterRules []policyRule
	roles := map[string]rbacObject{}
	bindings := map[string]rbacObject{}
	var lease rbacObject
	var sawLease bool
	for _, obj := range objs {
		switch obj.Kind {
		case "ClusterRole":
			clusterRules = append(clusterRules, obj.Rules...)
		case "Role":
			if obj.Metadata.Namespace == "kube-node-lease" {
				lease = obj
				sawLease = true
				continue
			}
			roles[obj.Metadata.Namespace] = obj
		case "RoleBinding":
			if obj.Metadata.Namespace == "kube-node-lease" {
				continue
			}
			bindings[obj.Metadata.Namespace] = obj
		}
	}
	for _, rule := range clusterRules {
		if hasResource(rule, "secrets") || hasResource(rule, "configmaps") || hasResource(rule, "serviceaccounts") || hasResource(rule, "serviceaccounts/token") {
			t.Fatalf("ClusterRole still grants %v %v", rule.Resources, rule.Verbs)
		}
		if hasResource(rule, "pods") && hasVerb(rule, "delete") {
			t.Fatalf("ClusterRole still deletes pods: %v", rule.Verbs)
		}
	}
	if !ruleHas(clusterRules, "", "pods", "get", "list", "watch") {
		t.Fatal("ClusterRole missing pods get/list/watch (virtual-kubelet lists with spec.nodeName at cluster scope)")
	}
	if ruleHas(clusterRules, "", "pods", "delete") || ruleHas(clusterRules, "", "pods", "create") {
		t.Fatal("ClusterRole pods rule includes create or delete")
	}
	if !ruleHas(clusterRules, "authentication.k8s.io", "tokenreviews", "create") {
		t.Fatal("ClusterRole missing tokenreviews create")
	}
	if !ruleHas(clusterRules, "authorization.k8s.io", "subjectaccessreviews", "create") {
		t.Fatal("ClusterRole missing subjectaccessreviews create")
	}

	for _, ns := range []string{"build", "ci"} {
		role, ok := roles[ns]
		if !ok {
			t.Fatalf("no Role in namespace %s", ns)
		}
		if !ruleExact(role.Rules, "", "secrets", "get") || !ruleExact(role.Rules, "", "configmaps", "get") {
			t.Fatalf("namespace %s secret/configmap rules: %+v", ns, role.Rules)
		}
		if ruleHas(role.Rules, "", "secrets", "list") || ruleHas(role.Rules, "", "secrets", "watch") {
			t.Fatalf("namespace %s secret rule lists or watches", ns)
		}
		if !ruleExact(role.Rules, "", "serviceaccounts/token", "create") {
			t.Fatalf("namespace %s missing serviceaccounts/token create: %+v", ns, role.Rules)
		}
		if !ruleExact(role.Rules, "", "pods", "delete") {
			t.Fatalf("namespace %s pod verbs: %+v", ns, role.Rules)
		}
		if !ruleExact(role.Rules, "", "pods/status", "update", "patch") {
			t.Fatalf("namespace %s pods/status: %+v", ns, role.Rules)
		}
		b, ok := bindings[ns]
		if !ok {
			t.Fatalf("no RoleBinding in namespace %s", ns)
		}
		if b.RoleRef.Kind != "Role" || b.RoleRef.Name != role.Metadata.Name {
			t.Fatalf("binding %s roleRef %+v", ns, b.RoleRef)
		}
		if len(b.Subjects) != 1 || b.Subjects[0].Kind != "ServiceAccount" || b.Subjects[0].Name != "darwin-node" || b.Subjects[0].Namespace != "darwin-system" {
			t.Fatalf("binding %s subjects %+v", ns, b.Subjects)
		}
	}
	if !sawLease {
		t.Fatal("no lease Role in kube-node-lease")
	}
	if !ruleExact(lease.Rules, "coordination.k8s.io", "leases", "get", "create", "update", "patch") {
		t.Fatalf("lease rules %+v", lease.Rules)
	}
	if ruleHas(lease.Rules, "coordination.k8s.io", "leases", "list") || ruleHas(lease.Rules, "coordination.k8s.io", "leases", "watch") || ruleHas(lease.Rules, "coordination.k8s.io", "leases", "delete") {
		t.Fatalf("lease rule is broader than the node lease controller: %+v", lease.Rules)
	}
}

func sensitiveRule(rule policyRule) bool {
	for _, r := range rule.Resources {
		switch r {
		case "secrets", "configmaps", "serviceaccounts", "serviceaccounts/token", "pods", "pods/status", "pods/log", "pods/exec", "pods/attach", "pods/portforward":
			return true
		}
	}
	return false
}

func hasResource(rule policyRule, resource string) bool {
	for _, r := range rule.Resources {
		if r == resource {
			return true
		}
	}
	return false
}

func hasVerb(rule policyRule, verb string) bool {
	for _, v := range rule.Verbs {
		if v == verb {
			return true
		}
	}
	return false
}

func ruleHas(rules []policyRule, group, resource string, verbs ...string) bool {
	for _, rule := range rules {
		if !hasResource(rule, resource) {
			continue
		}
		groupOK := len(rule.APIGroups) == 0 && group == ""
		for _, g := range rule.APIGroups {
			if g == group {
				groupOK = true
			}
		}
		if !groupOK {
			continue
		}
		ok := true
		for _, v := range verbs {
			if !hasVerb(rule, v) {
				ok = false
			}
		}
		if ok {
			return true
		}
	}
	return false
}

func ruleExact(rules []policyRule, group, resource string, verbs ...string) bool {
	for _, rule := range rules {
		if !hasResource(rule, resource) {
			continue
		}
		if len(rule.Verbs) != len(verbs) {
			continue
		}
		same := true
		for _, v := range verbs {
			if !hasVerb(rule, v) {
				same = false
			}
		}
		groupOK := len(rule.APIGroups) == 0 && group == ""
		for _, g := range rule.APIGroups {
			if g == group {
				groupOK = true
			}
		}
		if same && groupOK && len(rule.Resources) == 1 {
			return true
		}
	}
	return false
}

func loadChartValues(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(chartPath(t, "values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var vals map[string]any
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
	if err := dec.Decode(&vals); err != nil {
		t.Fatal(err)
	}
	return vals
}

func renderChart(t *testing.T, vals map[string]any) []rbacObject {
	t.Helper()
	objs, err := renderChartErr(vals)
	if err != nil {
		t.Fatal(err)
	}
	return objs
}

func renderChartErr(vals map[string]any) ([]rbacObject, error) {
	b, err := os.ReadFile(filepath.Join(chartDir(), "templates", "rbac.yaml"))
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New("rbac.yaml").Funcs(template.FuncMap{
		"fail": func(msg string) (string, error) {
			return "", fmt.Errorf("%s", msg)
		},
	}).Parse(string(b))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, chartCase{
		Values:  vals,
		Release: map[string]string{"Namespace": "darwin-system"},
	})
	if err != nil {
		return nil, err
	}
	return decodeRBAC(buf.String())
}

func decodeRBAC(s string) ([]rbacObject, error) {
	dec := utilyaml.NewYAMLOrJSONDecoder(strings.NewReader(s), 4096)
	var objs []rbacObject
	for {
		var obj rbacObject
		err := dec.Decode(&obj)
		if err == io.EOF {
			return objs, nil
		}
		if err != nil {
			return nil, err
		}
		if obj.Kind == "" {
			continue
		}
		objs = append(objs, obj)
	}
}

func chartDir() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		return ""
	}
	return filepath.Join(filepath.Dir(file), "..", "..", "deploy", "helm", "darwin-node")
}

func chartPath(t *testing.T, name string) string {
	t.Helper()
	dir := chartDir()
	if dir == "" {
		t.Fatal("runtime.Caller")
	}
	return filepath.Join(dir, name)
}
