package observability

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mlvea/darwin-node/pkg/runtime"
	rtfake "github.com/mlvea/darwin-node/pkg/runtime/fake"
	"github.com/mlvea/darwin-node/pkg/types"
)

func TestMetricsHandlerExposesGauges(t *testing.T) {
	m := NewMetrics()
	m.VMs.Set(1)
	m.Pods.Set(2)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	m.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "darwin_node_vms") {
		t.Fatalf("missing darwin_node_vms: %s", body)
	}
	if !strings.Contains(body, "darwin_node_pods") {
		t.Fatalf("missing darwin_node_pods: %s", body)
	}
}

func TestMetricsObserveSetsGauges(t *testing.T) {
	m := NewMetrics()
	m.Observe(func() (float64, float64) { return 2, 1 })
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	m.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "darwin_node_vms 2") {
		t.Fatalf("vms: %s", body)
	}
	if !strings.Contains(body, "darwin_node_pods 1") {
		t.Fatalf("pods: %s", body)
	}
}

func TestServeMetricsPath(t *testing.T) {
	m := NewMetrics()
	m.VMs.Set(1)
	srv := httptest.NewServer(metricsHTTP(m.Handler()))
	t.Cleanup(srv.Close)
	res, err := srv.Client().Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(b)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("status %d body %s", res.StatusCode, body)
	}
	if !strings.Contains(body, "darwin_node_vms") || !strings.Contains(body, "darwin_node_pods") {
		t.Fatalf("missing series: %s", body)
	}
}

func TestPullBootAndProbeSeries(t *testing.T) {
	m := NewMetrics()
	m.ObservePull(time.Second, nil)
	m.ObservePull(time.Second, errors.New("pull"))
	m.ObserveBoot(time.Second, nil)
	m.ObserveProbe("readiness", 10*time.Millisecond, nil)
	body := scrape(t, m)
	for _, name := range []string{
		"darwin_node_image_pulls_total",
		"darwin_node_image_pull_duration_seconds",
		"darwin_node_vm_boots_total",
		"darwin_node_vm_boot_duration_seconds",
		"darwin_node_probes_total",
		"darwin_node_probe_duration_seconds",
	} {
		if !strings.Contains(body, name) {
			t.Fatalf("missing %s\n%s", name, body)
		}
	}
}

func TestListenMetricsReportsBindError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	if _, err := ListenMetrics(ln.Addr().String(), NewMetrics().Handler()); err == nil {
		t.Fatal("bind error was dropped")
	}
}

func TestListenMetricsServes(t *testing.T) {
	m := NewMetrics()
	m.VMs.Set(1)
	stop, err := ListenMetrics("127.0.0.1:0", m.Handler())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
}

func TestMeterRecordsBootAndKeepsConsole(t *testing.T) {
	m := NewMetrics()
	rt := Meter(rtfake.New(), m)
	machine, err := rt.Create(context.Background(), types.VMSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := machine.(runtime.Consoler); !ok {
		t.Fatal("console machine lost Consoler")
	}
	if err := machine.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := scrape(t, m)
	if !strings.Contains(body, `darwin_node_vm_boots_total{result="ok"} 1`) {
		t.Fatalf("boot counter:\n%s", body)
	}
}

func scrape(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	return rec.Body.String()
}
