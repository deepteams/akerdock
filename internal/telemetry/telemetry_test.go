package telemetry

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

func TestFirstLine(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "no newline", in: "connection refused", want: "connection refused"},
		{name: "leading newline", in: "\nsecret details", want: ""},
		{name: "newline in the middle", in: "auth failed\npassword=hunter2", want: "auth failed"},
		{name: "trailing newline", in: "timeout\n", want: "timeout"},
		{name: "empty string", in: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := firstLine(tt.in); got != tt.want {
				t.Errorf("firstLine(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestSpanErrorRedactsToFirstLine(t *testing.T) {
	// Spans leave the instance: only the first line of a multi-line error
	// message may be recorded, the rest could carry a secret.
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	_, span := tp.Tracer("test").Start(context.Background(), "op")
	SpanError(span, errors.New("deploy failed\nDATABASE_URL=postgres://user:secret@host/db"))
	span.End()

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded spans = %d, want 1", len(spans))
	}
	events := spans[0].Events()
	if len(events) != 1 {
		t.Fatalf("span events = %d, want 1", len(events))
	}
	var recorded string
	for _, attr := range events[0].Attributes {
		if attr.Key == semconv.ExceptionMessageKey {
			recorded = attr.Value.AsString()
		}
	}
	if recorded != "deploy failed" {
		t.Errorf("recorded exception message = %q, want %q", recorded, "deploy failed")
	}
	if strings.Contains(recorded, "secret") {
		t.Errorf("second line leaked into the span: %q", recorded)
	}
}

func TestSpanErrorNilIsNoop(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer func() { _ = tp.Shutdown(context.Background()) }()

	_, span := tp.Tracer("test").Start(context.Background(), "op")
	SpanError(span, nil)
	span.End()

	spans := recorder.Ended()
	if len(spans) != 1 {
		t.Fatalf("recorded spans = %d, want 1", len(spans))
	}
	if n := len(spans[0].Events()); n != 0 {
		t.Errorf("span events after SpanError(nil) = %d, want 0", n)
	}
}

func TestSpanErrorOnNoopSpanDoesNotPanic(_ *testing.T) {
	// A span pulled from an empty context is a no-op span: SpanError must
	// still be safe to call on it.
	span := trace.SpanFromContext(context.Background())
	SpanError(span, errors.New("boom"))
	SpanError(span, nil)
}

func telemetryLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func preserveProviders(t *testing.T) {
	t.Helper()
	tracerProvider := otel.GetTracerProvider()
	meterProvider := otel.GetMeterProvider()
	t.Cleanup(func() {
		otel.SetTracerProvider(tracerProvider)
		otel.SetMeterProvider(meterProvider)
	})
}

func TestInitDisabledUsesNoopProviders(t *testing.T) {
	preserveProviders(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("AKERDOCK_METRICS_ENABLED", "")

	got := Init(context.Background(), "test", telemetryLogger())
	if got.Enabled() || got.Tracer == nil || got.Meter == nil || got.PromHandler != nil {
		t.Fatalf("disabled telemetry = %+v", got)
	}
	got.Shutdown(context.Background())
}

func TestInitPrometheusAndMetrics(t *testing.T) {
	preserveProviders(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("AKERDOCK_METRICS_ENABLED", "true")

	got := Init(context.Background(), "test", telemetryLogger())
	defer got.Shutdown(context.Background())
	if !got.Enabled() || got.PromHandler == nil {
		t.Fatalf("Prometheus telemetry not enabled: %+v", got)
	}

	metrics := NewMetrics(got.Meter)
	metrics.RecordJob(context.Background(), "deploy", "succeeded", 1.25)
	metrics.RecordDeployment(context.Background(), "succeeded", 2.5)
	var nilMetrics *Metrics
	nilMetrics.RecordJob(context.Background(), "ignored", "ignored", 0)
	nilMetrics.RecordDeployment(context.Background(), "ignored", 0)

	rec := httptest.NewRecorder()
	got.PromHandler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "akerdock_jobs_completed") ||
		!strings.Contains(rec.Body.String(), "akerdock_deployments_total") {
		t.Fatalf("metrics response = %d %q", rec.Code, rec.Body.String())
	}
}

func TestInitOTLPHTTP(t *testing.T) {
	preserveProviders(t)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
	t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", "http/protobuf")
	t.Setenv("AKERDOCK_METRICS_ENABLED", "")

	got := Init(context.Background(), "test", telemetryLogger())
	if !got.Enabled() || got.Tracer == nil || got.Meter == nil || len(got.shutdown) < 2 {
		t.Fatalf("OTLP providers were not built: %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got.Shutdown(ctx)
}

func TestShutdownCallsEveryProvider(t *testing.T) {
	var calls atomic.Int32
	telemetry := &Telemetry{shutdown: []func(context.Context) error{
		func(context.Context) error {
			calls.Add(1)
			return nil
		},
		func(context.Context) error {
			calls.Add(1)
			return errors.New("ignored")
		},
	}}
	telemetry.Shutdown(context.Background())
	if calls.Load() != 2 {
		t.Fatalf("shutdown calls = %d", calls.Load())
	}
}

func TestServiceName(t *testing.T) {
	old, present := os.LookupEnv("OTEL_SERVICE_NAME")
	t.Cleanup(func() {
		if present {
			_ = os.Setenv("OTEL_SERVICE_NAME", old)
		} else {
			_ = os.Unsetenv("OTEL_SERVICE_NAME")
		}
	})
	_ = os.Unsetenv("OTEL_SERVICE_NAME")
	if got := serviceName(); got != "akerdock" {
		t.Fatalf("default service name = %q", got)
	}
	_ = os.Setenv("OTEL_SERVICE_NAME", "control-plane")
	if got := serviceName(); got != "control-plane" {
		t.Fatalf("custom service name = %q", got)
	}
}
