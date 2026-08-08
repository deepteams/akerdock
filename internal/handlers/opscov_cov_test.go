package handlers

// Scaffolding + tests raising statement coverage of the ops-flavoured handler
// files (notifications, deployment logs, deployments). The opscovDB fake keeps
// the flowDB philosophy — sqlc still performs every generated Scan — but adds
// per-query steering: a step matched by the sqlc query name can fail, return no
// rows, change the Exec tag, or override individual scan destinations.

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/events"
	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/store"
)

// opscovStep steers every call whose SQL carries the given sqlc query name.
type opscovStep struct {
	name     string
	skip     int // matching calls to let through before activating
	err      error
	noRows   bool
	rows     int    // row count for Query (default 1)
	tag      string // Exec command tag (default "UPDATE 1")
	countOne bool   // scalar count queries answer 1 instead of 0
	fill     func(i int, dest any) bool
}

type opscovDB struct {
	steps    []*opscovStep
	truthy   bool
	countOne bool
}

// on registers a steering step for the named sqlc query (case-insensitive).
func (db *opscovDB) on(name string) *opscovStep {
	s := &opscovStep{name: strings.ToLower(name), rows: 1}
	db.steps = append(db.steps, s)
	return s
}

func (db *opscovDB) match(sql string) *opscovStep {
	low := strings.ToLower(sql)
	for _, s := range db.steps {
		if strings.Contains(low, "-- name: "+s.name+" ") {
			if s.skip > 0 {
				s.skip--
				return nil
			}
			return s
		}
	}
	return nil
}

func (db *opscovDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if s := db.match(sql); s != nil {
		if s.err != nil {
			return pgconn.CommandTag{}, s.err
		}
		tag := s.tag
		if tag == "" {
			tag = "UPDATE 1"
		}
		return pgconn.NewCommandTag(tag), nil
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (db *opscovDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	s := db.match(sql)
	if s != nil && s.err != nil {
		return nil, s.err
	}
	remaining := 1
	var fill func(int, any) bool
	if s != nil {
		remaining = s.rows
		if s.noRows {
			remaining = 0
		}
		fill = s.fill
	}
	return &opscovRows{remaining: remaining, truthy: db.truthy, fill: fill}, nil
}

func (db *opscovDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	s := db.match(sql)
	countOne := db.countOne
	var err error
	var fill func(int, any) bool
	if s != nil {
		err = s.err
		if s.noRows {
			err = pgx.ErrNoRows
		}
		countOne = countOne || s.countOne
		fill = s.fill
	}
	return opscovRow{
		err:        err,
		zeroScalar: strings.Contains(strings.ToLower(sql), "count(") && !countOne,
		truthy:     db.truthy,
		fill:       fill,
	}
}

type opscovRow struct {
	err        error
	zeroScalar bool
	truthy     bool
	fill       func(int, any) bool
}

func (r opscovRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, d := range dest {
		if r.fill != nil && r.fill(i, d) {
			continue
		}
		if err := fillScanDestination(d, r.zeroScalar, r.truthy); err != nil {
			return err
		}
	}
	return nil
}

type opscovRows struct {
	remaining int
	current   bool
	closed    bool
	err       error
	truthy    bool
	fill      func(int, any) bool
}

func (r *opscovRows) Close()                                       { r.closed = true }
func (r *opscovRows) Err() error                                   { return r.err }
func (r *opscovRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (r *opscovRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *opscovRows) Values() ([]any, error)                       { return nil, nil }
func (r *opscovRows) RawValues() [][]byte                          { return nil }
func (r *opscovRows) Conn() *pgx.Conn                              { return nil }
func (r *opscovRows) Next() bool {
	if r.closed || r.remaining == 0 {
		r.closed = true
		r.current = false
		return false
	}
	r.remaining--
	r.current = true
	return true
}

func (r *opscovRows) Scan(dest ...any) error {
	if !r.current {
		return errors.New("Scan called before Next")
	}
	for i, d := range dest {
		if r.fill != nil && r.fill(i, d) {
			continue
		}
		if err := fillScanDestination(d, false, r.truthy); err != nil {
			r.err = err
			r.Close()
			return err
		}
	}
	return nil
}

type opscovPool struct {
	db        *opscovDB
	beginErr  error
	commitErr error
}

func (p opscovPool) Begin(context.Context) (pgx.Tx, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}
	return &opscovTx{db: p.db, commitErr: p.commitErr}, nil
}
func (opscovPool) Ping(context.Context) error { return nil }

type opscovTx struct {
	db        *opscovDB
	commitErr error
}

func (t *opscovTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (t *opscovTx) Commit(context.Context) error          { return t.commitErr }
func (*opscovTx) Rollback(context.Context) error          { return nil }
func (*opscovTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 1, nil
}
func (*opscovTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return flowBatch{} }
func (*opscovTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (*opscovTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return &pgconn.StatementDescription{}, nil
}

func (t *opscovTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}

func (t *opscovTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}

func (t *opscovTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (*opscovTx) Conn() *pgx.Conn { return nil }

var (
	_ store.DBTX = (*opscovDB)(nil)
	_ pgx.Rows   = (*opscovRows)(nil)
	_ pgx.Tx     = (*opscovTx)(nil)
)

func opscovAPI(t *testing.T) (*API, *opscovDB) {
	t.Helper()
	db := &opscovDB{}
	q := store.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte("1:" + key + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return &API{
		Store:    q,
		Pool:     opscovPool{db: db},
		Settings: instance.NewCache(q),
		Keyring:  keyring,
		Audit:    &audit.Recorder{Store: q, Logger: logger},
		Events:   events.NewBroker(),
		Version:  "unit",
		Logger:   logger,
	}, db
}

// opscovIdentity builds a caller. Without arguments it is the most privileged
// fixture (root + instance root); with arguments it holds exactly those.
func opscovIdentity(perms ...auth.Permission) *auth.Identity {
	granted := []string{string(auth.PermRoot)}
	if len(perms) != 0 {
		granted = make([]string, 0, len(perms))
		for _, p := range perms {
			granted = append(granted, string(p))
		}
	}
	return &auth.Identity{
		TokenID: 1, TokenUUID: fixtureUUID, TeamID: 1, TeamUUID: fixtureUUID,
		Permissions:  granted,
		InstanceRoot: true,
		UserID:       ptr(int64(1)),
	}
}

func opscovRequest(method, target, body string) *http.Request {
	return opscovRequestAs(opscovIdentity(), method, target, body)
}

func opscovRequestAs(id *auth.Identity, method, target, body string) *http.Request {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	return req.WithContext(auth.WithIdentity(req.Context(), id))
}

// opscovEncrypt produces ciphertext the handler under test can decrypt: same
// keyring, same AAD, the fixture UUID every fake row carries.
func opscovEncrypt(t *testing.T, a *API, table, column string, plain []byte) []byte {
	t.Helper()
	enc, err := a.Keyring.Encrypt(table, column, fixtureUUID, plain)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// opscovBytesFill overrides every []byte scan destination with the given blob.
func opscovBytesFill(blob []byte) func(int, any) bool {
	return func(_ int, dest any) bool {
		if b, ok := dest.(*[]byte); ok {
			*b = blob
			return true
		}
		return false
	}
}

func opscovUnique() *pgconn.PgError { return &pgconn.PgError{Code: "23505"} }

func opscovBoom() error { return errors.New("opscov: store failure") }

// --- notifications.go -------------------------------------------------------

func TestOpscovChannelConfigMapsEveryBlock(t *testing.T) {
	cfg := channelConfig(
		ptr("https://example.test/hook"),
		&api.SmtpConfig{
			Host: "mail.example.test", From: "a@example.test", To: []string{"b@example.test"},
			Port: ptr(2525), Username: ptr("user"), Password: ptr("pass"),
			Encryption: ptr(api.SmtpConfigEncryption("starttls")),
		},
		&api.ResendConfig{ApiKey: "rk", From: "a@example.test", To: []string{"b@example.test"}},
		&api.TelegramConfig{BotToken: "bt", ChatId: "42", TopicId: ptr("7")},
		&api.PushoverConfig{Token: "pt", UserKey: "uk"},
	)
	if cfg.URL != "https://example.test/hook" {
		t.Errorf("URL = %q", cfg.URL)
	}
	if cfg.SMTP == nil || cfg.SMTP.Port != 2525 || cfg.SMTP.Username != "user" ||
		cfg.SMTP.Password != "pass" || cfg.SMTP.Encryption != "starttls" {
		t.Errorf("SMTP = %+v", cfg.SMTP)
	}
	if cfg.Resend == nil || cfg.Resend.APIKey != "rk" {
		t.Errorf("Resend = %+v", cfg.Resend)
	}
	if cfg.Telegram == nil || cfg.Telegram.TopicID != "7" {
		t.Errorf("Telegram = %+v", cfg.Telegram)
	}
	if cfg.Pushover == nil || cfg.Pushover.UserKey != "uk" {
		t.Errorf("Pushover = %+v", cfg.Pushover)
	}
}

func TestOpscovHHMMInvalidTimeIsAbsent(t *testing.T) {
	if got := hhmm(pgtype.Time{}); got != nil {
		t.Errorf("hhmm(invalid) = %v, want nil", got)
	}
}

func TestOpscovCreateNotificationChannel(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CreateNotificationChannel(rec, opscovRequest(http.MethodPost, "/api/v1/notification-channels", body), api.CreateNotificationChannelParams{})
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"kind":"webhook","name":"   ","url":"https://example.test"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("blank name = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"kind":"webhook","name":"ops"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("missing url = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"kind":"webhook","name":"ops","url":"https://example.test/h","enabled":false}`, nil); rec.Code != http.StatusCreated {
		t.Errorf("happy path = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"kind":"webhook","name":"ops","url":"https://example.test/h"}`, func(db *opscovDB) {
		db.on("CreateNotificationChannel").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", rec.Code)
	}
	if rec := run(t, `{"kind":"webhook","name":"ops","url":"https://example.test/h"}`, func(db *opscovDB) {
		db.on("CreateNotificationChannel").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
}

func TestOpscovGetNotificationChannelBadUUID(t *testing.T) {
	a, _ := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.GetNotificationChannel(rec, opscovRequest(http.MethodGet, "/api/v1/notification-channels/nope", ""), "not-a-uuid")
	if rec.Code != http.StatusNotFound {
		t.Errorf("malformed uuid = %d, want 404", rec.Code)
	}
}

func TestOpscovUpdateNotificationChannel(t *testing.T) {
	run := func(t *testing.T, ifMatch, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.UpdateNotificationChannel(rec, opscovRequest(http.MethodPatch, "/api/v1/notification-channels/"+fixtureUUID, body),
			fixtureUUID, api.UpdateNotificationChannelParams{IfMatch: ifMatch})
		return rec
	}

	if rec := run(t, "oops", `{}`, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad If-Match = %d, want 400", rec.Code)
	}
	if rec := run(t, `"1"`, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d, want 400", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"  "}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("blank name = %d, want 422", rec.Code)
	}
	// The channel kind fixture is webhook: a config block without a usable URL
	// must be refused for the kind the channel already is.
	if rec := run(t, `"1"`, `{"url":"not a url"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("invalid config = %d, want 422", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"ops","url":"https://example.test/h","enabled":false}`, nil); rec.Code != http.StatusOK {
		t.Errorf("happy path = %d, want 200: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `"1"`, `{"name":"ops"}`, func(db *opscovDB) {
		db.on("UpdateNotificationChannel").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate name = %d, want 409", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"ops"}`, func(db *opscovDB) {
		db.on("UpdateNotificationChannel").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("update failure = %d, want 500", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"ops"}`, func(db *opscovDB) {
		db.on("UpdateNotificationChannel").tag = "UPDATE 0"
	}); rec.Code != http.StatusConflict {
		t.Errorf("version conflict = %d, want 409", rec.Code)
	}
	if rec := run(t, `"1"`, `{"name":"ops"}`, func(db *opscovDB) {
		s := db.on("GetNotificationChannelByUUID")
		s.skip = 1 // let resolveChannel through, fail the reload
		s.err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("reload failure = %d, want 500", rec.Code)
	}
}

func TestOpscovDeleteNotificationChannelFailure(t *testing.T) {
	a, db := opscovAPI(t)
	db.on("DeleteNotificationChannel").err = opscovBoom()
	rec := httptest.NewRecorder()
	a.DeleteNotificationChannel(rec, opscovRequest(http.MethodDelete, "/api/v1/notification-channels/"+fixtureUUID, ""), fixtureUUID)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("delete failure = %d, want 500", rec.Code)
	}
}

func TestOpscovTestNotificationChannel(t *testing.T) {
	post := func(t *testing.T, prep func(*testing.T, *API, *opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(t, a, db)
		}
		rec := httptest.NewRecorder()
		a.TestNotificationChannel(rec, opscovRequest(http.MethodPost, "/api/v1/notification-channels/"+fixtureUUID+"/test", "{}"), fixtureUUID)
		return rec
	}

	// The default fixture blob is not a ciphertext at all.
	if rec := post(t, nil); rec.Code != http.StatusInternalServerError {
		t.Errorf("undecryptable config = %d, want 500", rec.Code)
	}
	// Decryptable, but not JSON.
	if rec := post(t, func(t *testing.T, a *API, db *opscovDB) {
		blob := opscovEncrypt(t, a, "notification_channels", "config_enc", []byte("not json"))
		db.on("GetNotificationChannelByUUID").fill = opscovBytesFill(blob)
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("non-JSON config = %d, want 500", rec.Code)
	}
	// A real config whose webhook target is a reserved address: the SSRF guard
	// refuses it without touching the network, and the handler reports the
	// failure as delivered=false rather than a 5xx.
	rec := post(t, func(t *testing.T, a *API, db *opscovDB) {
		blob := opscovEncrypt(t, a, "notification_channels", "config_enc", []byte(`{"url":"https://192.0.2.1/hook"}`))
		db.on("GetNotificationChannelByUUID").fill = opscovBytesFill(blob)
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("blocked send = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"delivered":false`) {
		t.Errorf("blocked send body = %s, want delivered:false", rec.Body)
	}
}

func TestOpscovCreateNotificationRule(t *testing.T) {
	run := func(t *testing.T, body string, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CreateNotificationRule(rec, opscovRequest(http.MethodPost, "/api/v1/notification-channels/"+fixtureUUID+"/rules", body), fixtureUUID)
		return rec
	}

	if rec := run(t, "{not json", nil); rec.Code != http.StatusBadRequest {
		t.Errorf("invalid JSON = %d, want 400", rec.Code)
	}
	if rec := run(t, `{"event_type":"  "}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("blank event_type = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"event_type":"deploy.failed.v1","environment_uuid":"`+fixtureUUID+`"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("environment without project = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"event_type":"deploy.failed.v1","debounce_seconds":-1}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("negative debounce = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"event_type":"deploy.failed.v1","quiet_hours_start":"25:99"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad quiet start = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"event_type":"deploy.failed.v1","quiet_hours_start":"22:00","quiet_hours_end":"nope"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad quiet end = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"event_type":"deploy.failed.v1","quiet_hours_start":"22:00"}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("half-open quiet window = %d, want 422", rec.Code)
	}
	if rec := run(t, `{"event_type":"deploy.failed.v1","digest_interval_minutes":0}`, nil); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("digest interval 0 = %d, want 422", rec.Code)
	}
	full := `{"event_type":"deploy.failed.v1","project_uuid":"` + fixtureUUID + `","environment_uuid":"` + fixtureUUID + `",` +
		`"debounce_seconds":30,"quiet_hours_start":"22:00","quiet_hours_end":"07:30",` +
		`"min_severity":"warning","enabled":false,"digest_enabled":true,"digest_interval_minutes":15}`
	if rec := run(t, full, nil); rec.Code != http.StatusCreated {
		t.Errorf("full rule = %d, want 201: %s", rec.Code, rec.Body)
	}
	if rec := run(t, `{"event_type":"deploy.failed.v1"}`, func(db *opscovDB) {
		db.on("CreateNotificationRule").err = opscovUnique()
	}); rec.Code != http.StatusConflict {
		t.Errorf("duplicate rule = %d, want 409", rec.Code)
	}
	if rec := run(t, `{"event_type":"deploy.failed.v1"}`, func(db *opscovDB) {
		db.on("CreateNotificationRule").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("store failure = %d, want 500", rec.Code)
	}
}

func TestOpscovDeleteNotificationRule(t *testing.T) {
	a, db := opscovAPI(t)
	db.on("GetNotificationRuleByUUID").noRows = true
	rec := httptest.NewRecorder()
	a.DeleteNotificationRule(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID, fixtureUUID)
	if rec.Code != http.StatusNotFound {
		t.Errorf("missing rule = %d, want 404", rec.Code)
	}

	a, db = opscovAPI(t)
	db.on("DeleteNotificationRule").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.DeleteNotificationRule(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID, fixtureUUID)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("delete failure = %d, want 500", rec.Code)
	}

	a, _ = opscovAPI(t)
	rec = httptest.NewRecorder()
	a.DeleteNotificationRule(rec, opscovRequest(http.MethodDelete, "/x", ""), fixtureUUID, "not-a-uuid")
	if rec.Code != http.StatusNotFound {
		t.Errorf("malformed rule uuid = %d, want 404", rec.Code)
	}
}

func TestOpscovListNotificationRulesFailure(t *testing.T) {
	a, db := opscovAPI(t)
	db.on("ListNotificationRules").err = opscovBoom()
	rec := httptest.NewRecorder()
	a.ListNotificationRules(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}

func TestOpscovListNotificationChannelsPaging(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListNotificationChannels(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListNotificationChannelsParams{Limit: ptr(0)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	a.ListNotificationChannels(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListNotificationChannelsParams{Cursor: ptr("!!!")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}

	db.on("ListNotificationChannelsPage").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListNotificationChannels(rec, opscovRequest(http.MethodGet, "/x", ""), api.ListNotificationChannelsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}

// --- deploymentlogs.go ------------------------------------------------------

func TestOpscovLogLinesDerivation(t *testing.T) {
	at := pgtype.Timestamptz{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true}
	steps := []store.DeploymentStep{
		{Name: "clone", Status: store.DeploymentStepStatusSkipped, StartedAt: at},
		{
			Name: "build", Status: store.DeploymentStepStatusFailed, StartedAt: at, FinishedAt: at,
			Log: ptr("line one\n\x1b[31mline two\x1b[0m\n"),
		},
		{Name: "push", Status: store.DeploymentStepStatusRunning}, // invalid timestamps -> now()
	}
	lines := logLines(steps)
	if len(lines) != 6 {
		t.Fatalf("len(lines) = %d, want 6", len(lines))
	}
	if lines[0].Message != "step clone: skipped" {
		t.Errorf("lines[0] = %q", lines[0].Message)
	}
	if lines[2].Message != "line one" || lines[3].Message != "line two" {
		t.Errorf("log lines = %q / %q — ANSI must be stripped", lines[2].Message, lines[3].Message)
	}
	if lines[4].Message != "step build: failed" {
		t.Errorf("lines[4] = %q", lines[4].Message)
	}
	for i, l := range lines {
		if l.Sequence != i+1 {
			t.Errorf("lines[%d].Sequence = %d", i, l.Sequence)
		}
	}
}

func TestOpscovGetDeploymentLogsPaging(t *testing.T) {
	logBlob := ptr("l1\nl2\nl3")
	fillLog := func(_ int, dest any) bool {
		if p, ok := dest.(**string); ok {
			*p = logBlob
			return true
		}
		if p, ok := dest.(*store.DeploymentStepStatus); ok {
			*p = store.DeploymentStepStatusSucceeded
			return true
		}
		return false
	}

	run := func(t *testing.T, params api.GetDeploymentLogsParams, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.GetDeploymentLogs(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID, params)
		return rec
	}

	if rec := run(t, api.GetDeploymentLogsParams{Cursor: ptr("abc")}, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}
	if rec := run(t, api.GetDeploymentLogsParams{Limit: ptr(0)}, nil); rec.Code != http.StatusBadRequest {
		t.Errorf("limit 0 = %d, want 400", rec.Code)
	}
	if rec := run(t, api.GetDeploymentLogsParams{}, func(db *opscovDB) {
		db.on("ListDeploymentSteps").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("steps failure = %d, want 500", rec.Code)
	}
	// One step with a 3-line log => 5 lines total; cursor=1, limit=1 => a full
	// page that is not the last one => next_cursor present.
	rec := run(t, api.GetDeploymentLogsParams{Cursor: ptr("1"), Limit: ptr(1)}, func(db *opscovDB) {
		db.on("ListDeploymentSteps").fill = fillLog
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("paged logs = %d, want 200: %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), `"next_cursor":"2"`) {
		t.Errorf("paged body = %s, want next_cursor 2", rec.Body)
	}
	if rec := run(t, api.GetDeploymentLogsParams{}, func(db *opscovDB) {
		db.on("GetDeploymentByUUIDForTeam").noRows = true
	}); rec.Code != http.StatusNotFound {
		t.Errorf("missing deployment = %d, want 404", rec.Code)
	}
}

// opscovPlainWriter is a ResponseWriter that is NOT an http.Flusher, to reach
// the streaming-unsupported branch.
type opscovPlainWriter struct {
	header http.Header
	code   int
	body   strings.Builder
}

func (w *opscovPlainWriter) Header() http.Header {
	if w.header == nil {
		w.header = http.Header{}
	}
	return w.header
}
func (w *opscovPlainWriter) WriteHeader(code int) { w.code = code }
func (w *opscovPlainWriter) Write(p []byte) (int, error) {
	return w.body.Write(p)
}

func TestOpscovStreamDeploymentLogs(t *testing.T) {
	sse := func(id *auth.Identity, lastEventID string) *http.Request {
		req := opscovRequestAs(id, http.MethodGet, "/x", "")
		req.Header.Set("Accept", "text/event-stream")
		if lastEventID != "" {
			req.Header.Set("Last-Event-ID", lastEventID)
		}
		return req
	}

	t.Run("no flusher", func(t *testing.T) {
		a, _ := opscovAPI(t)
		w := &opscovPlainWriter{}
		a.GetDeploymentLogs(w, sse(opscovIdentity(), ""), fixtureUUID, api.GetDeploymentLogsParams{})
		if w.code != http.StatusInternalServerError {
			t.Errorf("no-flusher code = %d, want 500", w.code)
		}
	})

	t.Run("terminal deployment ends the stream", func(t *testing.T) {
		a, db := opscovAPI(t)
		db.on("GetDeploymentByID").fill = func(_ int, dest any) bool {
			if p, ok := dest.(*store.DeploymentStatus); ok {
				*p = store.DeploymentStatusSucceeded
				return true
			}
			return false
		}
		rec := httptest.NewRecorder()
		a.GetDeploymentLogs(rec, sse(opscovIdentity(), "not-a-number"), fixtureUUID,
			api.GetDeploymentLogsParams{LastEventID: ptr("not-a-number")})
		if rec.Code != http.StatusOK {
			t.Fatalf("stream code = %d, want 200", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, "event: log") || !strings.Contains(body, "event: end") {
			t.Errorf("stream body = %q, want log + end events", body)
		}
	})

	t.Run("steps failure aborts silently", func(t *testing.T) {
		a, db := opscovAPI(t)
		db.on("ListDeploymentSteps").err = opscovBoom()
		rec := httptest.NewRecorder()
		a.GetDeploymentLogs(rec, sse(opscovIdentity(), "0"), fixtureUUID,
			api.GetDeploymentLogsParams{LastEventID: ptr("0")})
		if rec.Code != http.StatusOK {
			t.Errorf("stream code = %d, want 200 (headers already sent)", rec.Code)
		}
	})

	t.Run("cancelled client stops a non-terminal stream", func(t *testing.T) {
		a, _ := opscovAPI(t) // default status fixture "queued" is non-terminal
		req := sse(opscovIdentity(), "")
		ctx, cancel := context.WithCancel(req.Context())
		cancel()
		rec := httptest.NewRecorder()
		a.GetDeploymentLogs(rec, req.WithContext(ctx), fixtureUUID, api.GetDeploymentLogsParams{})
		if rec.Code != http.StatusOK {
			t.Errorf("stream code = %d, want 200", rec.Code)
		}
	})
}

// --- deployments.go ---------------------------------------------------------

func TestOpscovBrowsableRepo(t *testing.T) {
	cases := map[string]string{
		"":                                 "",
		"  git@github.com:acme/app.git  ":  "https://github.com/acme/app",
		"git@github.com:/acme/app":         "https://github.com/acme/app",
		"git@nowhere":                      "git@nowhere", // scp form without a path stays as-is
		"ssh://git@forge.example.test/o/r": "https://forge.example.test/o/r",
		"ssh://forge.example.test/o/r.git": "https://forge.example.test/o/r",
		"ssh://%gh&%ij":                    "ssh://%gh&%ij", // unparsable: unchanged
		"http://forge.example.test/o/r":    "https://forge.example.test/o/r",
		"https://forge.example.test/o/r/":  "https://forge.example.test/o/r",
	}
	for in, want := range cases {
		if got := browsableRepo(in); got != want {
			t.Errorf("browsableRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpscovGetDeploymentBadUUID(t *testing.T) {
	a, _ := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.GetDeployment(rec, opscovRequest(http.MethodGet, "/x", ""), "not-a-uuid")
	if rec.Code != http.StatusNotFound {
		t.Errorf("malformed uuid = %d, want 404", rec.Code)
	}
}

func TestOpscovCancelDeployment(t *testing.T) {
	statusFill := func(status store.DeploymentStatus) func(int, any) bool {
		return func(_ int, dest any) bool {
			if p, ok := dest.(*store.DeploymentStatus); ok {
				*p = status
				return true
			}
			return false
		}
	}
	run := func(t *testing.T, prep func(*opscovDB)) *httptest.ResponseRecorder {
		t.Helper()
		a, db := opscovAPI(t)
		if prep != nil {
			prep(db)
		}
		rec := httptest.NewRecorder()
		a.CancelDeployment(rec, opscovRequest(http.MethodPost, "/x", ""), fixtureUUID)
		return rec
	}

	if rec := run(t, func(db *opscovDB) {
		db.on("GetDeploymentByUUIDForTeam").fill = statusFill(store.DeploymentStatusSucceeded)
	}); rec.Code != http.StatusConflict {
		t.Errorf("terminal deployment = %d, want 409", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("GetDeploymentByUUIDForTeam").fill = statusFill(store.DeploymentStatusSwitching)
	}); rec.Code != http.StatusConflict {
		t.Errorf("switching deployment = %d, want 409", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("CancelQueuedDeployment").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("cancel failure = %d, want 500", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("RequestDeploymentJobCancel").err = opscovBoom()
	}); rec.Code != http.StatusInternalServerError {
		t.Errorf("job-cancel failure = %d, want 500", rec.Code)
	}
	if rec := run(t, nil); rec.Code != http.StatusAccepted {
		t.Errorf("queued deployment = %d, want 202", rec.Code)
	}
	if rec := run(t, func(db *opscovDB) {
		db.on("GetDeploymentByUUIDForTeam").noRows = true
	}); rec.Code != http.StatusNotFound {
		t.Errorf("missing deployment = %d, want 404", rec.Code)
	}
}

func TestOpscovListApplicationDeployments(t *testing.T) {
	a, db := opscovAPI(t)
	rec := httptest.NewRecorder()
	a.ListApplicationDeployments(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListApplicationDeploymentsParams{Limit: ptr(101)})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("limit 101 = %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	a.ListApplicationDeployments(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListApplicationDeploymentsParams{Cursor: ptr("###")})
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad cursor = %d, want 400", rec.Code)
	}

	db.on("ListDeploymentsForResource").err = opscovBoom()
	rec = httptest.NewRecorder()
	a.ListApplicationDeployments(rec, opscovRequest(http.MethodGet, "/x", ""), fixtureUUID,
		api.ListApplicationDeploymentsParams{})
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list failure = %d, want 500", rec.Code)
	}
}
