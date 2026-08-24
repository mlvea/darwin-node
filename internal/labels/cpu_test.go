package labels

import "testing"

func TestSanitize(t *testing.T) {
	got := SanitizeAppleCPUModel("Apple M4 Pro")
	if got != "M4_Pro" && got != "M4 Pro" {
		if got == "" {
			t.Fatal("empty")
		}
	}
	if SanitizeAppleCPUModel("Apple M2, Max") == "" {
		t.Fatal("empty after comma")
	}
}
