// Package audit records the append-only audit trail of §23.4 and the
// transactional outbox events of §24.2. Recording failures never fail the
// audited operation itself: they are logged and surfaced as metrics later.
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgtype"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/telemetry"
)

// Recorder writes audit and outbox rows.
type Recorder struct {
	Store  AuditStore
	Logger *slog.Logger
	// Metrics is optional: the audit trail is the chokepoint every action
	// crosses, so recording a counter + span event here instruments the whole
	// product from one place. Nil disables telemetry, never the audit row.
	Metrics *telemetry.Metrics
}

// telemetry emits the OTLP side of one audited action: a product-wide counter
// and an event on whatever span is active (the HTTP request span, or a job
// span). Governed by the signal toggles of the OTLP config — with metrics or
// traces off, the corresponding side is a no-op.
func (a *Recorder) telemetry(ctx context.Context, action, actor, result string) {
	a.Metrics.RecordAction(ctx, action, actor, result)
	if span := trace.SpanFromContext(ctx); span.SpanContext().IsValid() {
		span.AddEvent("akerdock.action", trace.WithAttributes(
			attribute.String("action", action),
			attribute.String("actor", actor),
			attribute.String("result", result),
		))
	}
}

// AuditStore is the generated-query boundary owned by this package. The
// recorder's formatting, redaction and failure policy are unit-testable
// independently from PostgreSQL; append-only SQL guarantees stay in the
// database module suite.
type AuditStore interface {
	InsertAuditEvent(context.Context, store.InsertAuditEventParams) error
}

type OutboxStore interface {
	InsertOutboxEvent(context.Context, store.InsertOutboxEventParams) error
}

var newUUID = pguuid.New

// Event is one audited action (§23.4 vocabulary: secret.reveal,
// server.delete, deployment.rollback, ...).
type Event struct {
	Action     string
	TargetKind string
	TargetUUID pgtype.UUID
	Result     store.AuditResult // defaults to success
	// Diff is what changed, already redacted by the caller (§23.4). It answers
	// "who changed what" — an audit log that only says "someone updated
	// something" is not an audit log.
	Diff map[string]any
}

// Record writes an audit event for an authenticated API request.
func (a *Recorder) Record(r *http.Request, id *auth.Identity, ev Event) {
	if ev.Result == "" {
		ev.Result = store.AuditResultSuccess
	}
	a.telemetry(r.Context(), ev.Action, "token", string(ev.Result))
	var actorUUID pgtype.UUID
	_ = actorUUID.Scan(id.TokenUUID)
	var teamID *int64
	if id.TeamID != 0 {
		teamID = &id.TeamID
	}
	var ip *netip.Addr
	params := store.InsertAuditEventParams{
		TeamID:     teamID,
		ActorKind:  store.ActorKindToken,
		ActorUuid:  actorUUID,
		Action:     ev.Action,
		TargetKind: strPtr(ev.TargetKind),
		TargetUuid: ev.TargetUUID,
		Result:     ev.Result,
		Ip:         ip,
		UserAgent:  strPtr(r.UserAgent()),
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if addr, err := netip.ParseAddr(host); err == nil {
			params.Ip = &addr
		}
	}
	if reqID := middleware.GetReqID(r.Context()); reqID != "" {
		// request ids from chi are not UUIDs; keep correlation via logs.
		_ = reqID
	}
	params.DiffRedacted = encodeDiff(ev.Diff, a.Logger)
	if err := a.Store.InsertAuditEvent(r.Context(), params); err != nil {
		a.Logger.Error("audit event lost", "action", ev.Action, "error", err)
	}
}

// RecordAuth writes an audit event for an authentication attempt — login,
// logout, MFA, passkey or OAuth (§23.4: login/logout/failures/MFA must be
// logged). Unlike Record, the actor is a USER, not an API token, and may be
// unresolved on failure: actorUUID is then zero and display carries the
// attempted identifier (e.g. the email typed at a failed login). teamID is nil
// when no team context is known.
func (a *Recorder) RecordAuth(r *http.Request, action string, result store.AuditResult, actorUUID pgtype.UUID, display string, teamID *int64) {
	if result == "" {
		result = store.AuditResultSuccess
	}
	a.telemetry(r.Context(), action, "user", string(result))
	params := store.InsertAuditEventParams{
		TeamID:       teamID,
		ActorKind:    store.ActorKindUser,
		ActorUuid:    actorUUID,
		ActorDisplay: strPtr(display),
		Action:       action,
		Result:       result,
		UserAgent:    strPtr(r.UserAgent()),
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if addr, err := netip.ParseAddr(host); err == nil {
			params.Ip = &addr
		}
	}
	if err := a.Store.InsertAuditEvent(r.Context(), params); err != nil {
		a.Logger.Error("audit event lost", "action", action, "error", err)
	}
}

// encodeDiff serializes a redacted diff. A diff that cannot be encoded is
// dropped rather than losing the audit event: the event itself matters more than
// its detail.
func encodeDiff(diff map[string]any, logger *slog.Logger) []byte {
	if len(diff) == 0 {
		return nil
	}
	raw, err := json.Marshal(diff)
	if err != nil {
		logger.Warn("audit diff dropped", "error", err)
		return nil
	}
	return raw
}

// SensitiveFields are the fields whose VALUE must never enter an audit diff —
// only the fact that they changed (INV-003, §23.4). An audit log that leaks the
// secret it is auditing is worse than no audit log: it is a second copy of the
// secret, in a table designed to be kept forever and exported.
var SensitiveFields = map[string]bool{
	"private_key": true, "value": true, "password": true, "secret": true,
	"secret_key": true, "access_key": true, "token": true, "url": true,
	"postgres_password": true, "config": true,
}

// Diff builds a redacted diff from a before/after pair of field values. A
// sensitive field is reported as changed, with its value replaced — never
// stored, never partially stored (a prefix of a secret is still a secret).
func Diff(before, after map[string]any) map[string]any {
	diff := map[string]any{}
	for key, newValue := range after {
		oldValue, existed := before[key]
		if existed && fmt.Sprint(oldValue) == fmt.Sprint(newValue) {
			continue
		}
		if SensitiveFields[key] {
			diff[key] = map[string]any{"changed": true, "redacted": true}
			continue
		}
		diff[key] = map[string]any{"from": oldValue, "to": newValue}
	}
	return diff
}

// System records an audited action performed by the system itself (jobs,
// bootstrap), outside any HTTP request.
func (a *Recorder) System(ctx context.Context, teamID *int64, action, targetKind string, targetUUID pgtype.UUID, result store.AuditResult) {
	if result == "" {
		result = store.AuditResultSuccess
	}
	a.telemetry(ctx, action, "system", string(result))
	if err := a.Store.InsertAuditEvent(ctx, store.InsertAuditEventParams{
		TeamID:     teamID,
		ActorKind:  store.ActorKindSystem,
		Action:     action,
		TargetKind: strPtr(targetKind),
		TargetUuid: targetUUID,
		Result:     result,
	}); err != nil {
		a.Logger.Error("audit event lost", "action", action, "error", err)
	}
}

// Outbox publishes a versioned domain event through the transactional
// outbox (§24.2). q may be a transaction-bound Queries so the event
// commits atomically with its mutation.
func (a *Recorder) Outbox(ctx context.Context, q OutboxStore, eventType string, teamUUID, resourceUUID pgtype.UUID, aggregateKey string, payload map[string]any) {
	u, err := newUUID()
	if err != nil {
		a.Logger.Error("outbox event lost", "event_type", eventType, "error", err)
		return
	}
	body, err := json.Marshal(payload)
	if err != nil || payload == nil {
		body = []byte("{}")
	}
	if err := q.InsertOutboxEvent(ctx, store.InsertOutboxEventParams{
		Uuid:         u,
		EventType:    eventType,
		TeamUuid:     teamUUID,
		ResourceUuid: resourceUUID,
		AggregateKey: strPtr(aggregateKey),
		Payload:      body,
	}); err != nil {
		a.Logger.Error("outbox event lost", "event_type", eventType, "error", err)
	}
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
