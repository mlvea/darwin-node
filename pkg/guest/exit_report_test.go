package guest

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"
)

func TestExitReportRequiresACode(t *testing.T) {
	if (ExitReport{}).Reported() {
		t.Fatal("omitted exitCode counted as a report")
	}
	var zero int32
	if !((ExitReport{ExitCode: &zero}).Reported()) {
		t.Fatal("exit code 0 was treated as omitted")
	}
	if got := NormalizeExitReason("OOMKilled"); got != "OOMKilled" {
		t.Fatalf("reason %q", got)
	}
	if got := NormalizeExitReason("not a reason"); got != "" {
		t.Fatalf("reason %q was accepted", got)
	}
	if got := NormalizeExitReason(""); got != "" {
		t.Fatal("empty reason")
	}
	if got := NormalizeExitReason(strings.Repeat("A", 129)); got != "" {
		t.Fatalf("129-character reason %q was accepted", got)
	}
}

func TestGuestExitNotifyReachesHost(t *testing.T) {
	a, b := net.Pipe()
	h := Handler{Token: "secret", AgentVersion: "test"}
	h.Init()
	go func() { _ = Serve(context.Background(), a, h) }()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cli, err := Dial(ctx, b, "secret", "host")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	got := make(chan ExitReport, 2)
	cli.OnExit(func(rep ExitReport) { got <- rep })

	h.ReportExit(ExitReport{Reason: "Error"})
	code := int32(137)
	h.ReportExit(ExitReport{ExitCode: &code, Reason: "OOMKilled"})
	zero := int32(0)
	select {
	case rep := <-got:
		if rep.ExitCode == nil || *rep.ExitCode != 137 || rep.Reason != "OOMKilled" {
			t.Fatalf("report %+v", rep)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("host did not observe the guest exit report")
	}
	h.ReportExit(ExitReport{ExitCode: &zero, Reason: "Completed"})
	select {
	case rep := <-got:
		if rep.ExitCode == nil || *rep.ExitCode != 0 || rep.Reason != "Completed" {
			t.Fatalf("zero report %+v", rep)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("exit code 0 was dropped")
	}
}
