// Package telemetry wires traces and metrics to an OTLP endpoint (ADR-008:
// "OTLP everywhere, no proprietary protocol").
//
// Configuration goes through the STANDARD OTEL_* variables, which is the one
// deliberate exception to the AKERDOCK_* prefix (instance-config §2.4): a
// proprietary variable duplicating OTEL_EXPORTER_OTLP_ENDPOINT would recreate
// the bespoke protocol ADR-008 rejects.
//
// Without OTEL_EXPORTER_OTLP_ENDPOINT, nothing is exported and nothing is
// attempted — no background retry loop, no repeated warning in the logs of the
// (many) instances that will never run a collector.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Telemetry owns the providers and shuts them down cleanly.
type Telemetry struct {
	Tracer trace.Tracer
	Meter  metric.Meter
	// PromHandler serves /metrics when the scrape endpoint is enabled; nil
	// otherwise, and the route stays unmounted.
	PromHandler http.Handler

	shutdown []func(context.Context) error
	enabled  bool
}

// Enabled reports whether an OTLP endpoint was configured.
func (t *Telemetry) Enabled() bool { return t.enabled }

// Init builds the providers. It never fails the boot: an unreachable collector
// must not stop a PaaS from deploying — telemetry is how you watch the system,
// not part of what the system does.
func Init(ctx context.Context, version string, logger *slog.Logger) *Telemetry {
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	promEnabled := os.Getenv("AKERDOCK_METRICS_ENABLED") == "true"
	if endpoint == "" && !promEnabled {
		// Neither export configured: hand back no-op providers. Instrumented
		// code stays unconditional, and nothing is attempted in the background.
		return &Telemetry{
			Tracer:  otel.Tracer(scopeName),
			Meter:   otel.Meter(scopeName),
			enabled: false,
		}
	}

	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL,
		semconv.ServiceName(serviceName()),
		semconv.ServiceVersion(version),
	))
	if err != nil {
		logger.Warn("telemetry: resource", "error", err)
		res = resource.Default()
	}

	t := &Telemetry{enabled: true}

	if endpoint != "" {
		// The exporters read the OTEL_* variables themselves (endpoint, headers,
		// protocol, TLS): the SDK is the contract, we do not re-parse it.
		traceExporter, err := otlptracehttp.New(ctx)
		if err != nil {
			logger.Warn("telemetry: trace exporter disabled", "error", err)
		} else {
			tp := sdktrace.NewTracerProvider(
				sdktrace.WithBatcher(traceExporter),
				sdktrace.WithResource(res),
			)
			otel.SetTracerProvider(tp)
			t.shutdown = append(t.shutdown, tp.Shutdown)
		}
	}

	// OTLP push and Prometheus scrape are two READERS on ONE meter provider —
	// not two providers. Instruments are declared once (ADR-008); only the way
	// out differs, and an operator can legitimately want both.
	var readers []sdkmetric.Option
	if endpoint != "" {
		if exp, err := otlpmetrichttp.New(ctx); err != nil {
			logger.Warn("telemetry: metric exporter disabled", "error", err)
		} else {
			readers = append(readers, sdkmetric.WithReader(
				sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(30*time.Second))))
		}
	}
	if promEnabled {
		registry := prom.NewRegistry()
		if exp, err := prometheus.New(prometheus.WithRegisterer(registry)); err != nil {
			logger.Warn("telemetry: prometheus endpoint disabled", "error", err)
		} else {
			readers = append(readers, sdkmetric.WithReader(exp))
			t.PromHandler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
			logger.Info("prometheus metrics exposed on /metrics")
		}
	}
	if len(readers) > 0 {
		mp := sdkmetric.NewMeterProvider(append(readers, sdkmetric.WithResource(res))...)
		otel.SetMeterProvider(mp)
		t.shutdown = append(t.shutdown, mp.Shutdown)
	}

	t.Tracer = otel.Tracer(scopeName)
	t.Meter = otel.Meter(scopeName)
	logger.Info("telemetry enabled", "otlp_endpoint", endpoint, "prometheus", promEnabled, "service", serviceName())
	return t
}

// Shutdown flushes what is buffered. Bounded: a collector that has gone away
// must not hold the process on its way out.
func (t *Telemetry) Shutdown(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for _, fn := range t.shutdown {
		_ = fn(ctx)
	}
}

const scopeName = "github.com/deepteams/akerdock"

func serviceName() string {
	if name := os.Getenv("OTEL_SERVICE_NAME"); name != "" {
		return name
	}
	return "akerdock"
}

// Metrics are the instruments the control plane reports. They are created once
// and reused: a metric created per call would leak an instrument per call.
type Metrics struct {
	JobsCompleted     metric.Int64Counter
	JobDuration       metric.Float64Histogram
	DeploymentsTotal  metric.Int64Counter
	DeploymentLatency metric.Float64Histogram
}

// NewMetrics builds the instruments. Errors are folded into no-ops rather than
// returned: a broken instrument must not break a deployment.
func NewMetrics(m metric.Meter) *Metrics {
	jobs, _ := m.Int64Counter("akerdock.jobs.completed",
		metric.WithDescription("Jobs that reached a terminal state, by type and status"))
	jobDur, _ := m.Float64Histogram("akerdock.job.duration",
		metric.WithDescription("Job execution time"), metric.WithUnit("s"))
	deploys, _ := m.Int64Counter("akerdock.deployments.total",
		metric.WithDescription("Deployments that reached a terminal state, by status"))
	deployDur, _ := m.Float64Histogram("akerdock.deployment.duration",
		metric.WithDescription("Deployment wall-clock time"), metric.WithUnit("s"))
	return &Metrics{
		JobsCompleted: jobs, JobDuration: jobDur,
		DeploymentsTotal: deploys, DeploymentLatency: deployDur,
	}
}

// RecordJob reports one terminal job.
func (m *Metrics) RecordJob(ctx context.Context, jobType, status string, seconds float64) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("job.type", jobType),
		attribute.String("job.status", status),
	)
	m.JobsCompleted.Add(ctx, 1, attrs)
	m.JobDuration.Record(ctx, seconds, attrs)
}

// RecordDeployment reports one terminal deployment.
func (m *Metrics) RecordDeployment(ctx context.Context, status string, seconds float64) {
	if m == nil {
		return
	}
	attrs := metric.WithAttributes(attribute.String("deployment.status", status))
	m.DeploymentsTotal.Add(ctx, 1, attrs)
	m.DeploymentLatency.Record(ctx, seconds, attrs)
}

// SpanError marks a span as failed with a message that must never carry a
// secret — spans leave the instance.
func SpanError(span trace.Span, err error) {
	if err == nil {
		return
	}
	span.RecordError(fmt.Errorf("%s", firstLine(err.Error())))
}

func firstLine(s string) string {
	for i, r := range s {
		if r == '\n' {
			return s[:i]
		}
	}
	return s
}
