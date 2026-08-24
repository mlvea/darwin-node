package observability

import (
	"context"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

func otlpInsecure(envInsecure, envEndpoint string) bool {
	return envInsecure == "true" || strings.HasPrefix(envEndpoint, "http://")
}

// StartOTLP installs a tracer provider when OTEL_EXPORTER_OTLP_ENDPOINT is set.
func StartOTLP(ctx context.Context, endpoint string) (func(context.Context) error, error) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	endpoint = strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://")
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(endpoint)}
	if otlpInsecure(os.Getenv("OTEL_EXPORTER_OTLP_INSECURE"), os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")) {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	svc := os.Getenv("OTEL_SERVICE_NAME")
	if svc == "" {
		svc = "darwin-node"
	}
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithMaxExportBatchSize(64), sdktrace.WithBatchTimeout(5*time.Second)),
		sdktrace.WithResource(resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName(svc))),
	)
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// LogStartError records exporter setup failure. Callers keep running.
func LogStartError(logf func(msg string, args ...any), err error, endpoint string) {
	if err == nil || logf == nil {
		return
	}
	logf("otel exporter setup failed", "err", err, "endpoint", endpoint)
}
