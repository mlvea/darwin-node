package main

import (
	"strings"
	"testing"
	"text/template"
)

func renderRuntimeClass(t *testing.T, vals map[string]any) string {
	t.Helper()
	tmpl, err := template.ParseFiles(chartPath(t, "templates/runtimeclass.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, map[string]any{"Values": vals}); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestRuntimeClassOffByDefault(t *testing.T) {
	vals := loadChartValues(t)
	got := renderRuntimeClass(t, vals)
	if strings.TrimSpace(got) != "" {
		t.Fatalf("default chart rendered a RuntimeClass:\n%s", got)
	}
	rc, _ := vals["runtimeClass"].(map[string]any)
	if rc == nil || rc["enabled"] != false || rc["name"] != "darwin-vm" {
		t.Fatalf("runtimeClass defaults = %#v", rc)
	}
}

func TestRuntimeClassCarriesStaticScheduling(t *testing.T) {
	vals := loadChartValues(t)
	rc := vals["runtimeClass"].(map[string]any)
	rc["enabled"] = true
	got := renderRuntimeClass(t, vals)
	for _, want := range []string{
		"kind: RuntimeClass",
		"name: darwin-vm",
		"kubernetes.io/os: darwin",
		"darwin.node/macos",
		"virtual-kubelet.io/provider",
		"darwin.node/nat-only",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered chart missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "MutatingAdmissionPolicy") {
		t.Fatal("slot-request policy rendered while admission.slotRequest is false")
	}

	rc["tolerateNAT"] = false
	got = renderRuntimeClass(t, vals)
	if strings.Contains(got, "darwin.node/nat-only") {
		t.Fatalf("NAT toleration rendered with tolerateNAT false:\n%s", got)
	}
}

func TestSlotRequestPolicyPatchesContainerZero(t *testing.T) {
	vals := loadChartValues(t)
	vals["admission"].(map[string]any)["slotRequest"] = true
	got := renderRuntimeClass(t, vals)
	for _, want := range []string{
		"kind: MutatingAdmissionPolicy",
		"kind: MutatingAdmissionPolicyBinding",
		"runtimeClassName == 'darwin-vm'",
		"darwin.node~1vm-slot",
		`{"requests": {"darwin.node/vm-slot": "1"}}`,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("rendered policy missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "kind: RuntimeClass") {
		t.Fatal("RuntimeClass rendered while runtimeClass.enabled is false")
	}
}
