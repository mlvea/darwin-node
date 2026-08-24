package observability

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
