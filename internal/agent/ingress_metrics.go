package agent

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/deepteams/akerdock/internal/telemetry"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// ingressMetrics keeps labels deliberately bounded: transport, outcome,
// direction and terminal reason are finite protocol enums. Endpoint and
// session UUIDs belong in logs/traces, never metric labels.
type ingressMetrics struct {
	sessions        metric.Int64Counter
	sessionEnds     metric.Int64Counter
	sessionFailures metric.Int64Counter
	streamOpens     metric.Int64Counter
	activeStreams   metric.Int64UpDownCounter
	openLatency     metric.Float64Histogram
	queueWait       metric.Float64Histogram
	bytes           metric.Int64Counter
}

func newIngressMetrics() *ingressMetrics {
	meter := otel.Meter(telemetry.ScopeName())
	sessions, _ := meter.Int64Counter("akerdock.ingress.sessions",
		metric.WithDescription("Ingress sessions successfully attached"))
	sessionEnds, _ := meter.Int64Counter("akerdock.ingress.session.ends",
		metric.WithDescription("Ingress session endings by transport and reason"))
	sessionFailures, _ := meter.Int64Counter("akerdock.ingress.session.failures",
		metric.WithDescription("Ingress sessions lost through transport failure"))
	streamOpens, _ := meter.Int64Counter("akerdock.ingress.stream.opens",
		metric.WithDescription("Ingress stream open attempts by outcome"))
	activeStreams, _ := meter.Int64UpDownCounter("akerdock.ingress.streams.active",
		metric.WithDescription("Open ingress transport streams, including reusable idle streams"))
	openLatency, _ := meter.Float64Histogram("akerdock.ingress.stream.open.duration",
		metric.WithDescription("Ingress stream admission and open handshake latency"), metric.WithUnit("s"))
	queueWait, _ := meter.Float64Histogram("akerdock.ingress.stream.queue.wait",
		metric.WithDescription("Ingress stream admission wait"), metric.WithUnit("s"))
	bytes, _ := meter.Int64Counter("akerdock.ingress.bytes",
		metric.WithDescription("Bytes relayed between the agent and the developer machine"), metric.WithUnit("By"))
	return &ingressMetrics{
		sessions: sessions, sessionEnds: sessionEnds, sessionFailures: sessionFailures,
		streamOpens: streamOpens, activeStreams: activeStreams,
		openLatency: openLatency, queueWait: queueWait, bytes: bytes,
	}
}

func transportAttrs(transport string) metric.MeasurementOption {
	return metric.WithAttributes(attribute.String("ingress.transport", transport))
}

func (m *ingressMetrics) recordSessionStart(ctx context.Context, transport string) {
	m.sessions.Add(ctx, 1, transportAttrs(transport))
}

func (m *ingressMetrics) recordSessionEnd(ctx context.Context, transport string, reason tunnel.EndReason) {
	attrs := metric.WithAttributes(
		attribute.String("ingress.transport", transport),
		attribute.String("ingress.reason", string(reason)),
	)
	m.sessionEnds.Add(ctx, 1, attrs)
	if reason == tunnel.EndDisconnect {
		m.sessionFailures.Add(ctx, 1, transportAttrs(transport))
	}
}

func (m *ingressMetrics) recordQueueWait(ctx context.Context, transport string, wait time.Duration, err error) {
	m.queueWait.Record(ctx, wait.Seconds(), metric.WithAttributes(
		attribute.String("ingress.transport", transport),
		attribute.String("ingress.outcome", ingressMetricOutcome(err)),
	))
}

func (m *ingressMetrics) recordStreamOpen(ctx context.Context, transport string, elapsed time.Duration, err error) {
	attrs := metric.WithAttributes(
		attribute.String("ingress.transport", transport),
		attribute.String("ingress.outcome", ingressMetricOutcome(err)),
	)
	m.streamOpens.Add(ctx, 1, attrs)
	m.openLatency.Record(ctx, elapsed.Seconds(), attrs)
}

func ingressMetricOutcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, tunnel.ErrOriginQueueFull):
		return "queue_full"
	case errors.Is(err, tunnel.ErrOriginQueueTimeout):
		return "queue_timeout"
	case errors.Is(err, tunnel.ErrOriginClosed):
		return "closed"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	default:
		return "failed"
	}
}

func (m *ingressMetrics) wrapStream(ctx context.Context, transport string, conn net.Conn) net.Conn {
	m.activeStreams.Add(ctx, 1, transportAttrs(transport))
	return &ingressMeasuredConn{
		Conn: conn, metrics: m, ctx: context.WithoutCancel(ctx), transport: transport,
	}
}

type ingressMeasuredConn struct {
	net.Conn
	metrics    *ingressMetrics
	ctx        context.Context
	transport  string
	readBytes  atomic.Int64
	wroteBytes atomic.Int64
	closeOnce  sync.Once
	closeErr   error
}

func (c *ingressMeasuredConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.readBytes.Add(int64(n))
	return n, err
}

func (c *ingressMeasuredConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.wroteBytes.Add(int64(n))
	return n, err
}

func (c *ingressMeasuredConn) Close() error {
	c.closeOnce.Do(func() {
		c.closeErr = c.Conn.Close()
		c.metrics.activeStreams.Add(c.ctx, -1, transportAttrs(c.transport))
		if n := c.readBytes.Load(); n > 0 {
			c.metrics.bytes.Add(c.ctx, n, metric.WithAttributes(
				attribute.String("ingress.transport", c.transport),
				attribute.String("ingress.direction", "laptop_to_agent"),
			))
		}
		if n := c.wroteBytes.Load(); n > 0 {
			c.metrics.bytes.Add(c.ctx, n, metric.WithAttributes(
				attribute.String("ingress.transport", c.transport),
				attribute.String("ingress.direction", "agent_to_laptop"),
			))
		}
	})
	return c.closeErr
}
