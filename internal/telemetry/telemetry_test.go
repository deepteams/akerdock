package telemetry

import (
	"context"
	"errors"
	"strings"
	"testing"

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
