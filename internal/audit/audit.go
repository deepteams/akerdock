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
	Store  Store
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

// Store is the generated-query boundary owned by this package. The
// recorder's formatting, redaction and failure policy are unit-testable
// independently from PostgreSQL; append-only SQL guarantees stay in the
// database module suite.
type Store interface {
	InsertAuditEvent(context.Context, store.InsertAuditEventParams) error
	// ResolveAuditTargetName turns the target's kind + uuid into its display
	// name, read HERE so the trail keeps the name the resource had at the time
	// of the action (00084). Best-effort: an unknown kind or a row already gone
	// leaves the name empty and the uuid speaks for itself.
	ResolveAuditTargetName(context.Context, store.ResolveAuditTargetNameParams) (string, error)
}

// OutboxStore persists domain events to the transactional outbox (§24.2).
type OutboxStore interface {
	InsertOutboxEvent(context.Context, store.InsertOutboxEventParams) error
}

var newUUID = pguuid.New

// ctxKey namespaces the request/correlation ids carried on the context so the
// audit rows can be correlated to a request and to a client-supplied chain
// (§23.4). Set by the request-id middleware.
type ctxKey int

const (
	requestIDKey ctxKey = iota
	correlationIDKey
)

// WithRequestID attaches a per-request UUID to the context.
func WithRequestID(ctx context.Context, id pgtype.UUID) context.Context {
	return context.WithValue(ctx, requestIDKey, id)
}

// WithCorrelationID attaches a correlation UUID (a client-supplied chain id) to
// the context.
func WithCorrelationID(ctx context.Context, id pgtype.UUID) context.Context {
	return context.WithValue(ctx, correlationIDKey, id)
}

func requestID(ctx context.Context) pgtype.UUID {
	id, _ := ctx.Value(requestIDKey).(pgtype.UUID)
	return id
}

func correlationID(ctx context.Context) pgtype.UUID {
	id, _ := ctx.Value(correlationIDKey).(pgtype.UUID)
	return id
}

// Event is one audited action (§23.4 vocabulary: secret.reveal,
// server.delete, deployment.rollback, ...).
type Event struct {
	Action     string
	TargetKind string
	TargetUUID pgtype.UUID
	// TargetName is the display name of the target. Left empty, it is resolved
	// from the kind and uuid; set it when the caller already knows the name, or
	// when the row is about to disappear (a deletion audited after the fact
	// would otherwise resolve to nothing).
	TargetName string
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
		TeamID:        teamID,
		ActorKind:     store.ActorKindToken,
		ActorUuid:     actorUUID,
		ActorDisplay:  strPtr(id.Display),
		Action:        ev.Action,
		TargetKind:    strPtr(ev.TargetKind),
		TargetUuid:    ev.TargetUUID,
		TargetName:    strPtr(a.targetName(r.Context(), ev)),
		Result:        ev.Result,
		Ip:            ip,
		UserAgent:     strPtr(r.UserAgent()),
		RequestID:     requestID(r.Context()),
		CorrelationID: correlationID(r.Context()),
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if addr, err := netip.ParseAddr(host); err == nil {
			params.Ip = &addr
		}
	}
	params.DiffRedacted = encodeDiff(ev.Diff, a.Logger)
	if err := a.Store.InsertAuditEvent(r.Context(), params); err != nil {
		a.Logger.Error("audit event lost", "action", ev.Action, "error", err)
	}
	a.securityAlert(r.Context(), id.TeamUUID, ev)
}

// targetName resolves what the trail should call the target: the caller's own
// label when it gave one, otherwise the resource's current name — read now,
// because "now" is when the action happened and the trail is never rewritten.
//
// A failure is not an error: `application 3f2a…` is a poorer line than
// `application varuna`, but a lost audit row would be far worse, so nothing
// here can fail the recording.
func (a *Recorder) targetName(ctx context.Context, ev Event) string {
	if ev.TargetName != "" {
		return ev.TargetName
	}
	if ev.TargetKind == "" || !ev.TargetUUID.Valid {
		return ""
	}
	name, err := a.Store.ResolveAuditTargetName(ctx, store.ResolveAuditTargetNameParams{
		TargetUuid: ev.TargetUUID, TargetKind: ev.TargetKind,
	})
	if err != nil {
		return ""
	}
	return name
}

// sensitiveActions maps a high-signal audited action to the security event type
// routed through the notification pipeline (ADR-019, SOC2 CC7.2). Kept to the
// team-scoped actions an operator wants to hear about immediately.
var sensitiveActions = map[string]string{
	"secret.reveal":      "security.secret_revealed.v1",
	"role.create":        "security.rbac_changed.v1",
	"role.update":        "security.rbac_changed.v1",
	"role.delete":        "security.rbac_changed.v1",
	"member.role.update": "security.rbac_changed.v1",
	"token.create":       "security.token_changed.v1",
	"token.revoke":       "security.token_changed.v1",
	"backup.restore":     "security.backup_restored.v1",
}

// securityAlert turns a sensitive audited action into a security.* outbox event
// so the existing notification rules deliver it (a team configures a rule on the
// event type). Best-effort: it never fails the audited action, and it no-ops
// when the store has no outbox (some tests).
func (a *Recorder) securityAlert(ctx context.Context, teamUUID string, ev Event) {
	eventType, ok := sensitiveActions[ev.Action]
	if !ok {
		return
	}
	outboxStore, ok := a.Store.(OutboxStore)
	if !ok || teamUUID == "" {
		return
	}
	var team pgtype.UUID
	if team.Scan(teamUUID) != nil {
		return
	}
	a.Outbox(ctx, outboxStore, eventType, team, ev.TargetUUID, ev.Action, map[string]any{
		"action": ev.Action,
		"result": string(ev.Result),
	})
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
		TeamID:        teamID,
		ActorKind:     store.ActorKindUser,
		ActorUuid:     actorUUID,
		ActorDisplay:  strPtr(display),
		Action:        action,
		Result:        result,
		UserAgent:     strPtr(r.UserAgent()),
		RequestID:     requestID(r.Context()),
		CorrelationID: correlationID(r.Context()),
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
		TargetName: strPtr(a.targetName(ctx, Event{
			TargetKind: targetKind, TargetUUID: targetUUID,
		})),
		Result:        result,
		RequestID:     requestID(ctx),
		CorrelationID: correlationID(ctx),
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
