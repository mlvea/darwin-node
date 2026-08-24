package labels

import (
	"regexp"
	"strings"
)

var invalid = regexp.MustCompile(`[^a-zA-Z0-9_.-]+`)

// SanitizeAppleCPUModel turns "Apple M4 Pro" into a valid k8s label value.
// Derived from Agoda macOS-vz-kubelet SanitizeAppleCPUModelForK8sLabel
// (Apache-2.0).
func SanitizeAppleCPUModel(name string) string {
	model := strings.ReplaceAll(name, "Apple ", "")
	model = invalid.ReplaceAllString(model, "_")
	return strings.Trim(model, "_-.")
}
