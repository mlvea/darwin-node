package guest

import (
	"testing"
	"time"
)

func TestCPUNanoCoresFromDelta(t *testing.T) {
	// 25 busy seconds of 100, on 4 cores: one core for the interval.
	got := cpuNanoCores(100, 400, 125, 500, 4)
	if got != 1_000_000_000 {
		t.Fatalf("cpu nano-cores %d, want 1000000000", got)
	}
	if got := cpuNanoCores(0, 0, 10, 10, 2); got != 0 {
		t.Fatalf("first sample %d, want 0", got)
	}
	if got := cpuNanoCores(50, 100, 40, 80, 2); got != 0 {
		t.Fatalf("clock went backwards: %d", got)
	}
}

func TestDefaultMetricsCPUAdvancesWhenBusy(t *testing.T) {
	if _, err := defaultMetrics(); err != nil {
		t.Fatal(err)
	}
	stop := time.Now().Add(300 * time.Millisecond)
	var sink uint64
	for time.Now().Before(stop) {
		sink++
	}
	if sink == 0 {
		t.Fatal("busy loop did not run")
	}
	m, err := defaultMetrics()
	if err != nil {
		t.Fatal(err)
	}
	if m.CPUNanoCores == 0 {
		t.Fatal("cpu stayed 0 while this process was busy")
	}
}
