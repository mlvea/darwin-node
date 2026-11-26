//go:build darwin && arm64

package provider

import (
	"os"
	"testing"
)

// TestNodeLifecycleHardwareReplay is the Mac Mini hook for the fault-free
// scripts in testdata/fuzz/node_lifecycle_hardware.jsonl. Linux CI replays
// those scripts against SimVZ inside FuzzNodeLifecycle. This process does
// not boot Virtualization.framework.
func TestNodeLifecycleHardwareReplay(t *testing.T) {
	st, err := os.Stat(hardwareReplayPath)
	if err != nil || st.IsDir() || st.Size() == 0 {
		t.Fatalf("hardware replay list %s: %v", hardwareReplayPath, err)
	}
	if os.Getenv("DARWIN_NODE_VZ_HARDWARE") != "1" {
		t.Skip("hardware-pending: replay " + hardwareReplayPath + " on a Mac Mini with Virtualization.framework")
	}
	t.Fatalf("hardware-pending: %s is the replay list; this test does not drive Virtualization.framework", hardwareReplayPath)
}
