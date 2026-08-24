package observability

import (
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// SetupSlog installs a JSON slog handler at the given level.
func SetupSlog(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	l := slog.New(h)
	slog.SetDefault(l)
	return l
}

// Metrics is the process-level registry.
type Metrics struct {
	Registry *prometheus.Registry
	VMs      prometheus.Gauge
	Pods     prometheus.Gauge

	mu      sync.Mutex
	observe func() (vms, pods float64)
}

func NewMetrics() *Metrics {
	r := prometheus.NewRegistry()
	m := &Metrics{
		Registry: r,
		VMs: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "darwin_node_vms",
			Help: "Currently held macOS VM slots (max 2).",
		}),
		Pods: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "darwin_node_pods",
			Help: "Pods known to the engine.",
		}),
	}
	r.MustRegister(m.VMs, m.Pods)
	return m
}

// Observe sets a scrape-time snapshot for VMs/Pods gauges.
func (m *Metrics) Observe(fn func() (vms, pods float64)) {
	m.mu.Lock()
	m.observe = fn
	m.mu.Unlock()
}

func (m *Metrics) Handler() http.Handler {
	inner := promhttp.HandlerFor(m.Registry, promhttp.HandlerOpts{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		fn := m.observe
		m.mu.Unlock()
		if fn != nil {
			vms, pods := fn()
			m.VMs.Set(vms)
			m.Pods.Set(pods)
		}
		inner.ServeHTTP(w, r)
	})
}

func metricsHTTP(h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/metrics", h)
	return mux
}

// ServeMetrics listens on addr and serves h at /metrics.
func ServeMetrics(addr string, h http.Handler) error {
	return http.ListenAndServe(addr, metricsHTTP(h))
}
