package audit

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/pguuid"
	"github.com/deepteams/akerdock/internal/store"
)

type fakeStore struct {
	auditParams  []store.InsertAuditEventParams
	auditErr     error
	outboxParams []store.InsertOutboxEventParams
	outboxErr    error
	// Name resolution: what the database would answer for the audited target,
	// and the lookups it was asked for.
	targetName  string
	targetErr   error
	nameLookups []store.ResolveAuditTargetNameParams
}

func (f *fakeStore) ResolveAuditTargetName(_ context.Context, arg store.ResolveAuditTargetNameParams) (string, error) {
	f.nameLookups = append(f.nameLookups, arg)
	return f.targetName, f.targetErr
}

func (f *fakeStore) InsertAuditEvent(_ context.Context, params store.InsertAuditEventParams) error {
	f.auditParams = append(f.auditParams, params)
	return f.auditErr
}

func (f *fakeStore) InsertOutboxEvent(_ context.Context, params store.InsertOutboxEventParams) error {
	f.outboxParams = append(f.outboxParams, params)
	return f.outboxErr
}

func testRecorder(store *fakeStore) *Recorder {
	return &Recorder{
		Store:  store,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestRecordEnrichesRequestAndActor(t *testing.T) {
	storeFake := &fakeStore{}
	recorder := testRecorder(storeFake)
	req := pguuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	corr := pguuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	ctx := WithCorrelationID(WithRequestID(context.Background(), req), corr)
	request := httptest.NewRequest(http.MethodPost, "/resource", nil).WithContext(ctx)

	recorder.Record(request, &auth.Identity{TokenUUID: "11111111-1111-4111-8111-111111111111", Display: "ci-token"},
		Event{Action: "application.deploy"})

	got := storeFake.auditParams[0]
	if got.RequestID != req {
		t.Errorf("request id = %v, want %v", got.RequestID, req)
	}
	if got.CorrelationID != corr {
		t.Errorf("correlation id = %v, want %v", got.CorrelationID, corr)
	}
	if got.ActorDisplay == nil || *got.ActorDisplay != "ci-token" {
		t.Errorf("actor display = %v, want the token name", got.ActorDisplay)
	}
}

func TestRecordEmitsSecurityAlert(t *testing.T) {
	storeFake := &fakeStore{}
	recorder := testRecorder(storeFake)
	req := httptest.NewRequest(http.MethodPost, "/secret/reveal", nil)
	id := &auth.Identity{TokenUUID: "11111111-1111-4111-8111-111111111111", TeamUUID: "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"}

	// A sensitive action emits a security.* outbox event…
	recorder.Record(req, id, Event{Action: "secret.reveal", Result: store.AuditResultSuccess})
	if len(storeFake.outboxParams) != 1 {
		t.Fatalf("outbox events = %d, want 1", len(storeFake.outboxParams))
	}
	if storeFake.outboxParams[0].EventType != "security.secret_revealed.v1" {
		t.Errorf("event type = %q", storeFake.outboxParams[0].EventType)
	}

	// …a routine action does not.
	recorder.Record(req, id, Event{Action: "application.update", Result: store.AuditResultSuccess})
	if len(storeFake.outboxParams) != 1 {
		t.Errorf("a routine action emitted a security alert: %d", len(storeFake.outboxParams))
	}
}

func TestRecordAuthSuccess(t *testing.T) {
	storeFake := &fakeStore{}
	recorder := testRecorder(storeFake)
	request := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	request.RemoteAddr = "203.0.113.5:5555"
	request.Header.Set("User-Agent", "unit-test")
	teamID := int64(7)
	recorder.RecordAuth(request, "auth.login", store.AuditResultSuccess,
		pguuid.MustParse("33333333-3333-4333-8333-333333333333"), "user@example.test", &teamID)

	if len(storeFake.auditParams) != 1 {
		t.Fatalf("insert count = %d", len(storeFake.auditParams))
	}
	got := storeFake.auditParams[0]
	if got.ActorKind != store.ActorKindUser {
		t.Errorf("actor kind = %q, want user", got.ActorKind)
	}
	if got.ActorDisplay == nil || *got.ActorDisplay != "user@example.test" {
		t.Errorf("actor display = %v, want the email", got.ActorDisplay)
	}
	if got.Result != store.AuditResultSuccess || got.Action != "auth.login" {
		t.Errorf("action/result = %q/%q", got.Action, got.Result)
	}
	if got.TeamID == nil || *got.TeamID != 7 {
		t.Errorf("team id = %v, want 7", got.TeamID)
	}
	if got.Ip == nil || got.Ip.String() != "203.0.113.5" {
		t.Errorf("ip = %v", got.Ip)
	}
}

func TestRecordAuthFailureHasNoActorUUID(t *testing.T) {
	storeFake := &fakeStore{}
	recorder := testRecorder(storeFake)
	request := httptest.NewRequest(http.MethodPost, "/auth/login", nil)
	request.RemoteAddr = "203.0.113.9:1111"
	// A failed login resolves nobody: zero uuid, only the attempted email, no team.
	recorder.RecordAuth(request, "auth.login", store.AuditResultFailure,
		pgtype.UUID{}, "attempt@example.test", nil)

	got := storeFake.auditParams[0]
	if got.Result != store.AuditResultFailure {
		t.Errorf("result = %q, want failure", got.Result)
	}
	if got.ActorUuid.Valid {
		t.Error("a failed login must not name an actor uuid")
	}
	if got.TeamID != nil {
		t.Error("a failed login has no team context")
	}
	if got.ActorDisplay == nil || *got.ActorDisplay != "attempt@example.test" {
		t.Errorf("actor display = %v, want the attempted email", got.ActorDisplay)
	}
}

func TestRecordMapsRequestIdentityAndDefaults(t *testing.T) {
	storeFake := &fakeStore{}
	recorder := testRecorder(storeFake)
	request := httptest.NewRequest(http.MethodPatch, "/resource", nil)
	request.RemoteAddr = "192.0.2.10:4321"
	request.Header.Set("User-Agent", "unit-test")
	request = request.WithContext(context.WithValue(request.Context(), middleware.RequestIDKey, "req-1"))
	identity := &auth.Identity{
		TokenUUID: "11111111-1111-4111-8111-111111111111",
		TeamID:    42,
	}
	recorder.Record(request, identity, Event{
		Action: "application.update", TargetKind: "application",
		TargetUUID: pguuid.MustParse("22222222-2222-4222-8222-222222222222"),
		Diff:       map[string]any{"name": "web"},
	})

	if len(storeFake.auditParams) != 1 {
		t.Fatalf("insert count = %d", len(storeFake.auditParams))
	}
	got := storeFake.auditParams[0]
	if got.Result != store.AuditResultSuccess || got.TeamID == nil || *got.TeamID != 42 ||
		got.Ip == nil || got.Ip.String() != "192.0.2.10" ||
		got.UserAgent == nil || *got.UserAgent != "unit-test" ||
		got.TargetKind == nil || *got.TargetKind != "application" ||
		!strings.Contains(string(got.DiffRedacted), `"web"`) {
		t.Fatalf("mapped audit params = %+v", got)
	}
}

func TestRecordHandlesOptionalFieldsAndStoreFailure(t *testing.T) {
	storeFake := &fakeStore{auditErr: errors.New("insert failed")}
	recorder := testRecorder(storeFake)
	request := httptest.NewRequest(http.MethodPost, "/", nil)
	request.RemoteAddr = "not-a-host-port"
	recorder.Record(request, &auth.Identity{}, Event{
		Action: "system.test", Result: store.AuditResultFailure,
	})
	got := storeFake.auditParams[0]
	if got.TeamID != nil || got.Ip != nil || got.TargetKind != nil ||
		got.Result != store.AuditResultFailure || got.DiffRedacted != nil {
		t.Fatalf("optional audit fields = %+v", got)
	}
}

func TestEncodeDiff(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if got := encodeDiff(nil, logger); got != nil {
		t.Fatalf("empty diff = %q", got)
	}
	if got := encodeDiff(map[string]any{"bad": make(chan int)}, logger); got != nil {
		t.Fatalf("unencodable diff should be dropped, got %q", got)
	}
	if got := encodeDiff(map[string]any{"name": "web"}, logger); !strings.Contains(string(got), `"web"`) {
		t.Fatalf("encoded diff = %q", got)
	}
}

func TestSystem(t *testing.T) {
	storeFake := &fakeStore{}
	recorder := testRecorder(storeFake)
	recorder.System(context.Background(), nil, "cleanup.run", "", pgtype.UUID{}, "")
	got := storeFake.auditParams[0]
	if got.ActorKind != store.ActorKindSystem || got.Result != store.AuditResultSuccess || got.TargetKind != nil {
		t.Fatalf("system event = %+v", got)
	}

	storeFake.auditErr = errors.New("insert failed")
	recorder.System(context.Background(), nil, "cleanup.failed", "server", pgtype.UUID{}, store.AuditResultFailure)
	if got := storeFake.auditParams[1]; got.Result != store.AuditResultFailure || got.TargetKind == nil {
		t.Fatalf("failed system event = %+v", got)
	}
}

func TestOutbox(t *testing.T) {
	storeFake := &fakeStore{}
	recorder := testRecorder(storeFake)
	team := pguuid.MustParse("11111111-1111-4111-8111-111111111111")
	resource := pguuid.MustParse("22222222-2222-4222-8222-222222222222")
	recorder.Outbox(context.Background(), storeFake, "server.updated.v1", team, resource, "server:1", map[string]any{"status": "ready"})
	got := storeFake.outboxParams[0]
	if !got.Uuid.Valid || got.AggregateKey == nil || *got.AggregateKey != "server:1" ||
		!strings.Contains(string(got.Payload), `"ready"`) {
		t.Fatalf("outbox params = %+v", got)
	}

	recorder.Outbox(context.Background(), storeFake, "empty.v1", team, resource, "", nil)
	if string(storeFake.outboxParams[1].Payload) != "{}" || storeFake.outboxParams[1].AggregateKey != nil {
		t.Fatalf("nil payload should become an empty object: %+v", storeFake.outboxParams[1])
	}

	recorder.Outbox(context.Background(), storeFake, "bad-payload.v1", team, resource, "", map[string]any{"bad": make(chan int)})
	if string(storeFake.outboxParams[2].Payload) != "{}" {
		t.Fatalf("unencodable payload should become an empty object: %q", storeFake.outboxParams[2].Payload)
	}

	storeFake.outboxErr = errors.New("insert failed")
	recorder.Outbox(context.Background(), storeFake, "failed.v1", team, resource, "", nil)
}

func TestOutboxUUIDFailure(t *testing.T) {
	old := newUUID
	newUUID = func() (pgtype.UUID, error) {
		return pgtype.UUID{}, errors.New("entropy unavailable")
	}
	t.Cleanup(func() { newUUID = old })

	storeFake := &fakeStore{}
	testRecorder(storeFake).Outbox(context.Background(), storeFake, "failed.v1", pgtype.UUID{}, pgtype.UUID{}, "", nil)
	if len(storeFake.outboxParams) != 0 {
		t.Fatal("an event without a UUID must not be inserted")
	}
}

func TestStrPtr(t *testing.T) {
	if strPtr("") != nil {
		t.Fatal("empty strings should map to nil")
	}
	if got := strPtr("value"); got == nil || *got != "value" {
		t.Fatalf("strPtr(value) = %v", got)
	}
}

// The trail records WHAT was touched, not only which row: `application varuna`
// where it used to say `application` and a uuid. The name is read when the
// entry is written, because an append-only log is never rewritten and the
// resource may be renamed — or deleted — the day after.
func TestRecordCapturesTheTargetName(t *testing.T) {
	storeFake := &fakeStore{targetName: "varuna"}
	recorder := testRecorder(storeFake)
	target := pguuid.MustParse("44444444-4444-4444-8444-444444444444")

	recorder.Record(httptest.NewRequest(http.MethodPost, "/applications", nil),
		&auth.Identity{TeamID: 1, TokenUUID: "11111111-1111-4111-8111-111111111111"},
		Event{Action: "application.update", TargetKind: "application", TargetUUID: target})

	if len(storeFake.nameLookups) != 1 ||
		storeFake.nameLookups[0].TargetKind != "application" ||
		storeFake.nameLookups[0].TargetUuid != target {
		t.Fatalf("target lookup = %#v", storeFake.nameLookups)
	}
	got := storeFake.auditParams[0]
	if got.TargetName == nil || *got.TargetName != "varuna" {
		t.Fatalf("target name = %v, want varuna", got.TargetName)
	}
}

// A caller that already knows the name — a hard delete, whose row is gone by
// the time it is audited — is believed, and costs no lookup.
func TestRecordPrefersTheCallerSuppliedName(t *testing.T) {
	storeFake := &fakeStore{targetName: "resolved-from-db"}
	recorder := testRecorder(storeFake)

	recorder.Record(httptest.NewRequest(http.MethodDelete, "/notification-channels/x", nil),
		&auth.Identity{TeamID: 1},
		Event{
			Action: "notification_channel.delete", TargetKind: "notification_channel",
			TargetUUID: pguuid.MustParse("55555555-5555-4555-8555-555555555555"),
			TargetName: "ops-slack",
		})

	if len(storeFake.nameLookups) != 0 {
		t.Errorf("a supplied name still queried the database: %#v", storeFake.nameLookups)
	}
	if got := storeFake.auditParams[0].TargetName; got == nil || *got != "ops-slack" {
		t.Fatalf("target name = %v, want ops-slack", got)
	}
}

// Naming is a nicety; the audit row is not. A failed lookup, a target with no
// name, or no target at all must all still write the entry.
func TestRecordSurvivesAnUnresolvableTarget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fake  *fakeStore
		event Event
	}{
		{
			"lookup fails", &fakeStore{targetErr: errors.New("gone")},
			Event{Action: "a", TargetKind: "application", TargetUUID: pguuid.MustParse("66666666-6666-4666-8666-666666666666")},
		},
		{"no target at all", &fakeStore{}, Event{Action: "instance.settings_updated"}},
		{"kind without a uuid", &fakeStore{}, Event{Action: "a", TargetKind: "instance"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := testRecorder(tc.fake)
			recorder.Record(httptest.NewRequest(http.MethodPost, "/x", nil), &auth.Identity{TeamID: 1}, tc.event)
			if len(tc.fake.auditParams) != 1 {
				t.Fatalf("audit rows = %d, want 1", len(tc.fake.auditParams))
			}
			if got := tc.fake.auditParams[0].TargetName; got != nil {
				t.Errorf("target name = %v, want none", *got)
			}
		})
	}
	// A target with no uuid must not even be looked up.
	quiet := &fakeStore{}
	testRecorder(quiet).Record(httptest.NewRequest(http.MethodPost, "/x", nil),
		&auth.Identity{TeamID: 1}, Event{Action: "a", TargetKind: "instance"})
	if len(quiet.nameLookups) != 0 {
		t.Errorf("looked up a target that has no uuid: %#v", quiet.nameLookups)
	}
}
