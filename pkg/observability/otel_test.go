package observability

import "testing"

func TestLogStartError(t *testing.T) {
	var got string
	LogStartError(func(msg string, args ...any) { got = msg }, errSentinel{}, "localhost:4317")
	if got != "otel exporter setup failed" {
		t.Fatalf("got %q", got)
	}
	got = ""
	LogStartError(func(msg string, args ...any) { got = msg }, nil, "x")
	if got != "" {
		t.Fatal("nil err must not log")
	}
}

type errSentinel struct{}

func (errSentinel) Error() string { return "boom" }

func TestOTLPInsecure(t *testing.T) {
	if !otlpInsecure("true", "") {
		t.Fatal("env")
	}
	if !otlpInsecure("", "http://localhost:4317") {
		t.Fatal("http endpoint")
	}
	if otlpInsecure("", "https://otel.example:4317") {
		t.Fatal("https must stay secure")
	}
}
