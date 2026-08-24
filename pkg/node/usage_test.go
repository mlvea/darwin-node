package node

import "testing"

func TestUsageNanoCoresNotCapacity(t *testing.T) {
	if UsageNanoCores(14, 0) != 0 {
		t.Fatal("idle must not report logical CPU count as nano-cores")
	}
	got := UsageNanoCores(14, 50)
	want := uint64(7e9)
	if got != want {
		t.Fatalf("got %d want %d", got, want)
	}
}

func TestUsageBytesNotCapacity(t *testing.T) {
	if UsageBytes(16<<30, 0) != 0 {
		t.Fatal("zero frac must not report capacity as usage")
	}
	got := UsageBytes(100, 0.5)
	if got != 50 {
		t.Fatalf("got %d", got)
	}
}
