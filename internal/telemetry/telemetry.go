// Package telemetry wires traces, metrics and logs to an OTLP endpoint
// (ADR-008: "OTLP everywhere, no proprietary protocol").
//
// The endpoint can come from the standard OTEL_* variables (the one deliberate
// exception to the AKERDOCK_* prefix, instance-config §2.4) OR from the
// instance settings stored in the database (encrypted, §14.2). The caller
// resolves which and hands a Config to Init; the settings-driven config takes
// effect at the next restart, since Init runs once at boot.
//
// With no endpoint and no Prometheus scrape, nothing is exported and nothing is
// attempted — no background retry, no repeated warning in the logs of the
// (many) instances that will never run a collector.
package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	prom "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/prometheus"
	logglobal "go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Config is the resolved OTLP export configuration handed to Init. It carries
// json tags because it is exactly what the instance settings persist (encrypted).
type Config struct {
	// Endpoint is the OTLP collector URL (scheme decides TLS); empty disables OTLP.
	Endpoint string `json:"endpoint"`
	// Protocol is "http" (default) or "grpc".
	Protocol string `json:"protocol"`
	// Headers are sent on every export (e.g. an auth token).
	Headers map[string]string `json:"headers,omitempty"`
	// Which signals to export.
	Traces  bool `json:"traces"`
	Metrics bool `json:"metrics"`
	Logs    bool `json:"logs"`
	// PromEnabled exposes the local /metrics scrape; from env, never persisted.
	PromEnabled bool `json:"-"`
}

// EnvConfig reads the fallback configuration from the standard OTEL_* variables.
// Used when no OTLP config is stored in the database. All three signals are on
// when an endpoint is present — env users opt in by setting the endpoint at all.
func EnvConfig() Config {
	return Config{
		Endpoint:    os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
		Protocol:    envOr("OTEL_EXPORTER_OTLP_PROTOCOL", "http"),
		Headers:     parseHeaders(os.Getenv("OTEL_EXPORTER_OTLP_HEADERS")),
		Traces:      true,
		Metrics:     true,
		Logs:        true,
		PromEnabled: os.Getenv("AKERDOCK_METRICS_ENABLED") == "true",
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// parseHeaders reads "k1=v1,k2=v2" as the OTEL_EXPORTER_OTLP_HEADERS convention.
func parseHeaders(s string) map[string]string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	out := map[string]string{}
	for _, pair := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(pair, "="); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

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

// Enabled reports whether any exporter was configured.
func (t *Telemetry) Enabled() bool { return t.enabled }

// Init builds the providers. It never fails the boot: an unreachable collector
// must not stop a PaaS from deploying — telemetry is how you watch the system,
// not part of what the system does.
func Init(ctx context.Context, version string, cfg Config, logger *slog.Logger) *Telemetry {
	if cfg.Protocol == "" {
		cfg.Protocol = "http"
	}
	if cfg.Endpoint == "" && !cfg.PromEnabled {
		// Nothing configured: hand back no-op providers. Instrumented code stays
		// unconditional, and nothing is attempted in the background.
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

	if cfg.Endpoint != "" && cfg.Traces {
		if exp, err := newTraceExporter(ctx, cfg); err != nil {
			logger.Warn("telemetry: trace exporter disabled", "error", err)
		} else {
			tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
			otel.SetTracerProvider(tp)
			t.shutdown = append(t.shutdown, tp.Shutdown)
		}
	}

	// OTLP push and Prometheus scrape are two READERS on ONE meter provider —
	// not two providers. Instruments are declared once (ADR-008); only the way
	// out differs, and an operator can legitimately want both.
	var readers []sdkmetric.Option
	if cfg.Endpoint != "" && cfg.Metrics {
		if exp, err := newMetricExporter(ctx, cfg); err != nil {
			logger.Warn("telemetry: metric exporter disabled", "error", err)
		} else {
			readers = append(readers, sdkmetric.WithReader(
				sdkmetric.NewPeriodicReader(exp, sdkmetric.WithInterval(30*time.Second))))
		}
	}
	if cfg.PromEnabled {
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

	// Logs: the LoggerProvider is set GLOBALLY so the slog bridge (attached to
	// the app logger at startup) starts exporting once it exists — before that
	// it targets the no-op global provider and drops nothing on the floor.
	if cfg.Endpoint != "" && cfg.Logs {
		if exp, err := newLogExporter(ctx, cfg); err != nil {
			logger.Warn("telemetry: log exporter disabled", "error", err)
		} else {
			lp := sdklog.NewLoggerProvider(
				sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)),
				sdklog.WithResource(res),
			)
			logglobal.SetLoggerProvider(lp)
			t.shutdown = append(t.shutdown, lp.Shutdown)
		}
	}

	t.Tracer = otel.Tracer(scopeName)
	t.Meter = otel.Meter(scopeName)
	logger.Info("telemetry enabled",
		"otlp_endpoint", cfg.Endpoint, "protocol", cfg.Protocol,
		"traces", cfg.Traces, "metrics", cfg.Metrics, "logs", cfg.Logs,
		"prometheus", cfg.PromEnabled, "service", serviceName())
	return t
}

// newTraceExporter picks the http or grpc OTLP trace exporter per cfg.Protocol.
func newTraceExporter(ctx context.Context, cfg Config) (sdktrace.SpanExporter, error) {
	if cfg.Protocol == "grpc" {
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpointURL(cfg.Endpoint)}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlptracegrpc.WithHeaders(cfg.Headers))
		}
		return otlptracegrpc.New(ctx, opts...)
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpointURL(cfg.Endpoint)}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlptracehttp.WithHeaders(cfg.Headers))
	}
	return otlptracehttp.New(ctx, opts...)
}

func newMetricExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	if cfg.Protocol == "grpc" {
		opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithEndpointURL(cfg.Endpoint)}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlpmetricgrpc.WithHeaders(cfg.Headers))
		}
		return otlpmetricgrpc.New(ctx, opts...)
	}
	opts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpointURL(cfg.Endpoint)}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlpmetrichttp.WithHeaders(cfg.Headers))
	}
	return otlpmetrichttp.New(ctx, opts...)
}

func newLogExporter(ctx context.Context, cfg Config) (sdklog.Exporter, error) {
	if cfg.Protocol == "grpc" {
		opts := []otlploggrpc.Option{otlploggrpc.WithEndpointURL(cfg.Endpoint)}
		if len(cfg.Headers) > 0 {
			opts = append(opts, otlploggrpc.WithHeaders(cfg.Headers))
		}
		return otlploggrpc.New(ctx, opts...)
	}
	opts := []otlploghttp.Option{otlploghttp.WithEndpointURL(cfg.Endpoint)}
	if len(cfg.Headers) > 0 {
		opts = append(opts, otlploghttp.WithHeaders(cfg.Headers))
	}
	return otlploghttp.New(ctx, opts...)
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

// ScopeName is the instrumentation scope, shared with the slog bridge.
func ScopeName() string { return scopeName }

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
	// ActionsTotal counts every audited action (the chokepoint every mutation
	// passes through), by action name, actor kind and result — the product-wide
	// "what happened" counter.
	ActionsTotal metric.Int64Counter
	// DockerOps counts every typed command sent on an agent channel
	// (ADR-052), by method and outcome — the migration's health signal.
	DockerOps metric.Int64Counter
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
	actions, _ := m.Int64Counter("akerdock.actions.total",
		metric.WithDescription("Audited actions, by action, actor and result"))
	dockerOps, _ := m.Int64Counter("akerdock.docker.runtime.ops",
		metric.WithDescription("Typed Docker commands sent on agent channels, by method and outcome"))
	return &Metrics{
		JobsCompleted: jobs, JobDuration: jobDur,
		DeploymentsTotal: deploys, DeploymentLatency: deployDur,
		ActionsTotal: actions, DockerOps: dockerOps,
	}
}

// RecordAction reports one audited action — the single instrument behind
// "instrument every AkerDock action", fed from the audit chokepoint.
func (m *Metrics) RecordAction(ctx context.Context, action, actor, result string) {
	if m == nil || m.ActionsTotal == nil {
		return
	}
	m.ActionsTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("action", action),
		attribute.String("actor", actor),
		attribute.String("result", result),
	))
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

// RecordDockerOp reports one typed command sent on an agent channel
// (ADR-052): the migration's health signal, by method and outcome ("ok" or
// the wire error code).
func (m *Metrics) RecordDockerOp(ctx context.Context, method, outcome string) {
	if m == nil || m.DockerOps == nil {
		return
	}
	m.DockerOps.Add(ctx, 1, metric.WithAttributes(
		attribute.String("method", method),
		attribute.String("outcome", outcome),
	))
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
