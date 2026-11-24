package observability

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

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

	Pulls         *prometheus.CounterVec
	PullDuration  prometheus.Histogram
	Boots         *prometheus.CounterVec
	BootDuration  prometheus.Histogram
	Probes        *prometheus.CounterVec
	ProbeDuration *prometheus.HistogramVec

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
		Pulls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "darwin_node_image_pulls_total",
			Help: "Image pulls started by the cache. A cache hit is not a pull.",
		}, []string{"result"}),
		PullDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "darwin_node_image_pull_duration_seconds",
			Help:    "Wall time of one image pull, including failures.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 16),
		}),
		Boots: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "darwin_node_vm_boots_total",
			Help: "VM Start calls.",
		}, []string{"result"}),
		BootDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "darwin_node_vm_boot_duration_seconds",
			Help:    "Wall time of VM Start, including failures.",
			Buckets: prometheus.ExponentialBuckets(0.05, 2, 16),
		}),
		Probes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "darwin_node_probes_total",
			Help: "Probe samples. probe is startup, readiness, liveness, or sidecar.",
		}, []string{"probe", "result"}),
		ProbeDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "darwin_node_probe_duration_seconds",
			Help:    "Wall time of one probe sample.",
			Buckets: prometheus.ExponentialBuckets(0.005, 2, 14),
		}, []string{"probe"}),
	}
	r.MustRegister(m.VMs, m.Pods, m.Pulls, m.PullDuration, m.Boots, m.BootDuration, m.Probes, m.ProbeDuration)
	return m
}

func resultLabel(err error) string {
	if err == nil {
		return "ok"
	}
	return "error"
}

// ObservePull records one singleflight pull. Cache hits do not call it.
func (m *Metrics) ObservePull(d time.Duration, err error) {
	if m == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	m.PullDuration.Observe(d.Seconds())
	m.Pulls.WithLabelValues(resultLabel(err)).Inc()
}

// ObserveBoot records one VM Start.
func (m *Metrics) ObserveBoot(d time.Duration, err error) {
	if m == nil {
		return
	}
	if d < 0 {
		d = 0
	}
	m.BootDuration.Observe(d.Seconds())
	m.Boots.WithLabelValues(resultLabel(err)).Inc()
}

// ObserveProbe records one probe sample. kind is startup, readiness,
// liveness, or sidecar.
func (m *Metrics) ObserveProbe(kind string, d time.Duration, err error) {
	if m == nil {
		return
	}
	if kind == "" {
		kind = "unknown"
	}
	if d < 0 {
		d = 0
	}
	m.ProbeDuration.WithLabelValues(kind).Observe(d.Seconds())
	m.Probes.WithLabelValues(kind, resultLabel(err)).Inc()
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

// ListenMetrics binds addr and serves h at /metrics. The returned
// function shuts the server down. A bind error is returned to the
// caller; it is not discarded. ReadHeaderTimeout bounds a slow client.
func ListenMetrics(addr string, h http.Handler) (func(context.Context) error, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("metrics listen %s: %w", addr, err)
	}
	srv := &http.Server{Handler: metricsHTTP(h), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		err := srv.Serve(ln)
		if err != nil && err != http.ErrServerClosed {
			slog.Error("metrics server", "err", err)
		}
	}()
	return srv.Shutdown, nil
}
