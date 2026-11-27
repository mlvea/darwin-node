//go:build linux

package supervisor

import "testing"

func TestStartTicksIgnoresComm(t *testing.T) {
	stat := []byte("12 (my proc) S 1 1 1 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 4242 0\n")
	got, err := startTicks(stat)
	if err != nil {
		t.Fatal(err)
	}
	if got != 4242 {
		t.Fatalf("start ticks %d, want 4242", got)
	}
}
