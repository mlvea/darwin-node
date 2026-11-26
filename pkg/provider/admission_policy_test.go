package provider

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
)

func TestValidatingAdmissionPolicyParses(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "admission", "validatingadmissionpolicy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(b), 4096)
	var policy admissionregistrationv1.ValidatingAdmissionPolicy
	if err := dec.Decode(&policy); err != nil {
		t.Fatal(err)
	}
	var binding admissionregistrationv1.ValidatingAdmissionPolicyBinding
	if err := dec.Decode(&binding); err != nil {
		t.Fatal(err)
	}
	if policy.APIVersion != "admissionregistration.k8s.io/v1" || policy.Kind != "ValidatingAdmissionPolicy" {
		t.Fatalf("policy type %s %s", policy.APIVersion, policy.Kind)
	}
	if policy.Name != "darwin-node-pod-profile" {
		t.Fatalf("policy name %q", policy.Name)
	}
	if binding.Kind != "ValidatingAdmissionPolicyBinding" || binding.Spec.PolicyName != policy.Name {
		t.Fatalf("binding kind %s policyName %q", binding.Kind, binding.Spec.PolicyName)
	}
	deny := false
	for _, action := range binding.Spec.ValidationActions {
		if action == admissionregistrationv1.Deny {
			deny = true
		}
	}
	if !deny {
		t.Fatalf("validationActions %v, want Deny", binding.Spec.ValidationActions)
	}
	if policy.Spec.FailurePolicy == nil || *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		t.Fatalf("failurePolicy %v", policy.Spec.FailurePolicy)
	}
	if policy.Spec.MatchConstraints == nil {
		t.Fatal("missing matchConstraints")
	}
	var create, update, pods bool
	for _, rule := range policy.Spec.MatchConstraints.ResourceRules {
		for _, op := range rule.Operations {
			switch op {
			case admissionregistrationv1.Create:
				create = true
			case admissionregistrationv1.Update:
				update = true
			}
		}
		for _, res := range rule.Resources {
			if res == "pods" {
				pods = true
			}
		}
	}
	if !create || !update || !pods {
		t.Fatalf("resource rules %+v, want pods CREATE and UPDATE", policy.Spec.MatchConstraints.ResourceRules)
	}
	if len(policy.Spec.MatchConditions) == 0 {
		t.Fatal("policy is not scoped")
	}
	text := string(b)
	for _, needle := range []string{
		"darwin-vm",
		"kubernetes.io/os",
		"beta.kubernetes.io/os",
		"UnsupportedPodField",
		"UnsupportedVolume",
		"spec.hostNetwork",
		"spec.hostPID",
		"spec.hostIPC",
		"securityContext.privileged",
		"securityContext.capabilities.add",
		"seccompProfile",
		"RuntimeDefault",
		"appArmorProfile",
		"seLinuxOptions",
		"procMount",
		"Unmasked",
		"volumeDevices",
		"Never",
		"OnFailure",
		"Always",
		"allowlist",
	} {
		if !strings.Contains(text, needle) {
			t.Fatalf("policy file missing %q", needle)
		}
	}
	if len(policy.Spec.Validations) == 0 {
		t.Fatal("no validations")
	}
	for i, v := range policy.Spec.Validations {
		if strings.TrimSpace(v.Expression) == "" {
			t.Fatalf("validations[%d] has an empty expression", i)
		}
		if v.Reason == nil || *v.Reason != metav1.StatusReasonInvalid {
			t.Fatalf("validations[%d] reason %v", i, v.Reason)
		}
		if !strings.Contains(v.Message, "UnsupportedPodField") && !strings.Contains(v.Message, "UnsupportedVolume") {
			t.Fatalf("validations[%d] message %q", i, v.Message)
		}
	}
	assertVolumeCEL(t, text, reflect.TypeOf(corev1.VolumeSource{}), "v", map[string]bool{
		"hostPath": true, "emptyDir": true, "secret": true, "configMap": true, "projected": true, "downwardAPI": true,
	})
	assertVolumeCEL(t, text, reflect.TypeOf(corev1.VolumeProjection{}), "s", map[string]bool{
		"secret": true, "downwardAPI": true, "configMap": true, "serviceAccountToken": true,
	})
}

func assertVolumeCEL(t *testing.T, text string, rt reflect.Type, recv string, allowed map[string]bool) {
	t.Helper()
	seen := map[string]bool{}
	for i := 0; i < rt.NumField(); i++ {
		sf := rt.Field(i)
		if sf.PkgPath != "" {
			continue
		}
		name := jsonName(sf)
		seen[name] = true
		okCall := "has(" + recv + "." + name + ")"
		badCall := "!" + okCall
		if allowed[name] {
			if strings.Contains(text, badCall) {
				t.Fatalf("policy rejects supported %s.%s", recv, name)
			}
			if !strings.Contains(text, okCall) {
				t.Fatalf("policy does not admit %s.%s", recv, name)
			}
			continue
		}
		if !strings.Contains(text, badCall) {
			t.Fatalf("policy does not reject %s.%s", recv, name)
		}
	}
	for name := range allowed {
		if !seen[name] {
			t.Fatalf("%s no longer has %s", rt.Name(), name)
		}
	}
}
