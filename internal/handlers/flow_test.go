package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/events"
	"github.com/deepteams/akerdock/internal/instance"
	"github.com/deepteams/akerdock/internal/store"
)

const fixtureUUID = "11111111-1111-4111-8111-111111111111"

// flowDB is a deliberately small PostgreSQL protocol fake. sqlc still performs
// every generated Scan, so the HTTP tests cover the real store-to-API mapping;
// only the socket and PostgreSQL planner are replaced.
type flowDB struct {
	err      error
	truthy   bool
	countOne bool
	noRows   bool
}

func (db *flowDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	if db.err != nil {
		return pgconn.CommandTag{}, db.err
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (db *flowDB) Query(context.Context, string, ...any) (pgx.Rows, error) {
	if db.err != nil {
		return nil, db.err
	}
	remaining := 1
	if db.noRows {
		remaining = 0
	}
	return &flowRows{remaining: remaining, truthy: db.truthy}, nil
}

func (db *flowDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	err := db.err
	if db.noRows {
		err = pgx.ErrNoRows
	}
	return flowRow{
		err:        err,
		zeroScalar: strings.Contains(strings.ToLower(sql), "count(") && !db.countOne,
		truthy:     db.truthy,
	}
}

type flowRow struct {
	err        error
	zeroScalar bool
	truthy     bool
}

func (r flowRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for _, d := range dest {
		if err := fillScanDestination(d, r.zeroScalar, r.truthy); err != nil {
			return err
		}
	}
	return nil
}

type flowRows struct {
	remaining int
	current   bool
	closed    bool
	err       error
	truthy    bool
}

func (r *flowRows) Close()                                       { r.closed = true }
func (r *flowRows) Err() error                                   { return r.err }
func (r *flowRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (r *flowRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *flowRows) Values() ([]any, error)                       { return nil, nil }
func (r *flowRows) RawValues() [][]byte                          { return nil }
func (r *flowRows) Conn() *pgx.Conn                              { return nil }
func (r *flowRows) Next() bool {
	if r.closed || r.remaining == 0 {
		r.closed = true
		r.current = false
		return false
	}
	r.remaining--
	r.current = true
	return true
}
func (r *flowRows) Scan(dest ...any) error {
	if !r.current {
		return errors.New("Scan called before Next")
	}
	for _, d := range dest {
		if err := fillScanDestination(d, false, r.truthy); err != nil {
			r.err = err
			r.Close()
			return err
		}
	}
	return nil
}

var enumFixtures = map[string]string{
	"ActorKind":                  "token",
	"AdoptionScanStatus":         "completed",
	"ArtifactKind":               "local_image",
	"AuditResult":                "success",
	"BackupExecutionStatus":      "succeeded",
	"BuildPack":                  "image",
	"CertificateKind":            "letsencrypt",
	"CertificateStatus":          "valid",
	"DbEngine":                   "postgresql",
	"DeploymentStatus":           "queued",
	"DeploymentStepStatus":       "succeeded",
	"DeploymentTrigger":          "manual",
	"GitProvider":                "github",
	"GitSourceKind":              "public",
	"JobStatus":                  "dead_letter",
	"LogDrainKind":               "webhook",
	"MfaType":                    "totp",
	"NotificationChannelKind":    "webhook",
	"NotificationDeliveryStatus": "delivered",
	"NotificationSeverity":       "info",
	"OauthProvider":              "github",
	"PreviewProtection":          "none",
	"PreviewStatus":              "ready",
	"ProxyDesiredState":          "running",
	"ProxyRevisionStatus":        "active",
	"ProxyType":                  "traefik",
	"PublicAccessMode":           "public",
	"RedirectDirection":          "www_to_root",
	"ResourceDesiredStatus":      "running",
	"ResourceObservedStatus":     "running",
	"ResourceType":               "application",
	"RestoreDrillStatus":         "succeeded",
	"ServerStatus":               "ready",
	"SharedVariableScope":        "team",
	"StorageKind":                "volume",
	"TaskExecutionStatus":        "succeeded",
	"TaskMissedRunPolicy":        "skip",
	"TaskOverlapPolicy":          "forbid",
	"TeamRole":                   "owner",
	"TerminalEndReason":          "client_close",
	"TerminalTarget":             "server",
	"UptimeCheckKind":            "http",
	"UptimeStatus":               "up",
	"WebhookDeliveryStatus":      "processed",
	"WebhookProvider":            "github",
}

func fillScanDestination(dest any, zeroScalar, truthy bool) error {
	if dest == nil {
		return nil
	}
	switch d := dest.(type) {
	case *time.Time:
		*d = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		return nil
	case *netip.Addr:
		*d = netip.MustParseAddr("192.0.2.1")
		return nil
	case *netip.Prefix:
		*d = netip.MustParsePrefix("192.0.2.0/24")
		return nil
	case *pgtype.UUID:
		_ = d.Scan(fixtureUUID)
		return nil
	case *pgtype.Timestamptz:
		*d = pgtype.Timestamptz{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true}
		return nil
	case *pgtype.Timestamp:
		*d = pgtype.Timestamp{Time: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), Valid: true}
		return nil
	case *pgtype.Date:
		*d = pgtype.Date{Time: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Valid: true}
		return nil
	case *pgtype.Time:
		*d = pgtype.Time{Microseconds: int64(time.Hour / time.Microsecond), Valid: true}
		return nil
	case *pgtype.Text:
		*d = pgtype.Text{String: "unit", Valid: true}
		return nil
	case *pgtype.Bool:
		*d = pgtype.Bool{Bool: truthy, Valid: true}
		return nil
	case *pgtype.Int2:
		*d = pgtype.Int2{Int16: chooseInt16(zeroScalar), Valid: true}
		return nil
	case *pgtype.Int4:
		*d = pgtype.Int4{Int32: chooseInt32(zeroScalar), Valid: true}
		return nil
	case *pgtype.Int8:
		*d = pgtype.Int8{Int64: chooseInt64(zeroScalar), Valid: true}
		return nil
	case *pgtype.Float4:
		*d = pgtype.Float4{Float32: 1, Valid: true}
		return nil
	case *pgtype.Float8:
		*d = pgtype.Float8{Float64: 1, Valid: true}
		return nil
	case *pgtype.Numeric:
		*d = pgtype.Numeric{Int: big.NewInt(1), Valid: true}
		return nil
	}

	v := reflect.ValueOf(dest)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return errors.New("scan destination is not a non-nil pointer")
	}
	return fillValue(v.Elem(), zeroScalar, truthy)
}

func fillValue(v reflect.Value, zeroScalar, truthy bool) error {
	if !v.CanSet() {
		return nil
	}
	if v.Kind() == reflect.Pointer {
		v.Set(reflect.New(v.Type().Elem()))
		return fillValue(v.Elem(), zeroScalar, truthy)
	}
	if fixture, ok := enumFixtures[v.Type().Name()]; ok && v.Kind() == reflect.String {
		v.SetString(fixture)
		return nil
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString("unit")
	case reflect.Bool:
		v.SetBool(truthy)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(chooseInt64(zeroScalar))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.Slice:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			v.SetBytes([]byte("{}"))
		} else {
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))
		}
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
	case reflect.Struct:
		// sqlc nullable enum wrappers all expose a Valid field plus the enum.
		if valid := v.FieldByName("Valid"); valid.IsValid() && valid.CanSet() && valid.Kind() == reflect.Bool {
			valid.SetBool(true)
			for i := 0; i < v.NumField(); i++ {
				if v.Type().Field(i).Name != "Valid" {
					_ = fillValue(v.Field(i), zeroScalar, truthy)
				}
			}
		}
	}
	return nil
}

func chooseInt64(zero bool) int64 {
	if zero {
		return 0
	}
	return 1
}

func chooseInt32(zero bool) int32 { return int32(chooseInt64(zero)) }
func chooseInt16(zero bool) int16 { return int16(chooseInt64(zero)) }

type flowPool struct {
	db *flowDB
}

func (p flowPool) Begin(context.Context) (pgx.Tx, error) { return &flowTx{db: p.db}, nil }
func (flowPool) Ping(context.Context) error              { return nil }

type flowTx struct {
	db *flowDB
}

func (t *flowTx) Begin(context.Context) (pgx.Tx, error) { return t, nil }
func (*flowTx) Commit(context.Context) error            { return nil }
func (*flowTx) Rollback(context.Context) error          { return nil }
func (*flowTx) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	return 1, nil
}
func (*flowTx) SendBatch(context.Context, *pgx.Batch) pgx.BatchResults { return flowBatch{} }
func (*flowTx) LargeObjects() pgx.LargeObjects                         { return pgx.LargeObjects{} }
func (*flowTx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return &pgconn.StatementDescription{}, nil
}
func (t *flowTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return t.db.Exec(ctx, sql, args...)
}
func (t *flowTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return t.db.Query(ctx, sql, args...)
}
func (t *flowTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return t.db.QueryRow(ctx, sql, args...)
}
func (*flowTx) Conn() *pgx.Conn { return nil }

type flowBatch struct{}

func (flowBatch) Exec() (pgconn.CommandTag, error) { return pgconn.NewCommandTag("UPDATE 1"), nil }
func (flowBatch) Query() (pgx.Rows, error)         { return &flowRows{remaining: 1}, nil }
func (flowBatch) QueryRow() pgx.Row                { return flowRow{} }
func (flowBatch) Close() error                     { return nil }

// Compile-time guards catch pgx protocol changes in one obvious place.
var (
	_ store.DBTX       = (*flowDB)(nil)
	_ pgx.Rows         = (*flowRows)(nil)
	_ pgx.Tx           = (*flowTx)(nil)
	_ pgx.BatchResults = flowBatch{}
	_                  = pgproto3.FieldDescription{}
)

func flowAPI(t *testing.T) (*API, *flowDB) {
	t.Helper()
	db := &flowDB{}
	q := store.New(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte("1:" + key + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return &API{
		Store:    q,
		Pool:     flowPool{db: db},
		Settings: instance.NewCache(q),
		Keyring:  keyring,
		Audit:    &audit.Recorder{Store: q, Logger: logger},
		Events:   events.NewBroker(),
		Version:  "unit",
		Logger:   logger,
	}, db
}

type flowOperation struct {
	method string
	path   string
	item   *openapi3.PathItem
	op     *openapi3.Operation
}

func flowOperations(t *testing.T) []flowOperation {
	t.Helper()
	spec, err := openapi3.NewLoader().LoadFromFile("../../docs/specs/openapi-v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var operations []flowOperation
	for path, item := range spec.Paths.Map() {
		for method, op := range item.Operations() {
			if op.Extensions["x-required-permission"] == nil {
				continue
			}
			if op.OperationID == "streamEvents" {
				// SSE is covered with an explicitly cancelled request below;
				// it must not keep this finite request matrix open.
				continue
			}
			operations = append(operations, flowOperation{method: strings.ToUpper(method), path: path, item: item, op: op})
		}
	}
	return operations
}

func requestForFlow(t *testing.T, flow flowOperation) *http.Request {
	t.Helper()
	target := concreteURL(flow.path)
	values := url.Values{}
	for _, ref := range append(flow.item.Parameters, flow.op.Parameters...) {
		p := ref.Value
		if p == nil || p.In != "query" || !p.Required {
			continue
		}
		values.Set(p.Name, scalarString(schemaFixture(p.Name, p.Schema)))
	}
	if len(values) != 0 {
		target += "?" + values.Encode()
	}

	body := any(map[string]any{})
	if flow.op.RequestBody != nil && flow.op.RequestBody.Value != nil {
		if media := flow.op.RequestBody.Value.GetMediaType("application/json"); media != nil {
			body = schemaFixture(flow.op.OperationID, media.Schema)
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(flow.method, target, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("If-Match", `"1"`)
	return req
}

func scalarString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int:
		return "1"
	case float64:
		return "1"
	default:
		return "unit"
	}
}

func schemaFixture(name string, ref *openapi3.SchemaRef) any {
	if ref == nil || ref.Value == nil {
		return "unit"
	}
	s := ref.Value
	if s.Example != nil {
		return s.Example
	}
	if s.Default != nil {
		return s.Default
	}
	if len(s.Enum) != 0 {
		return s.Enum[0]
	}
	if len(s.OneOf) != 0 {
		return schemaFixture(name, s.OneOf[0])
	}
	if len(s.AnyOf) != 0 {
		return schemaFixture(name, s.AnyOf[0])
	}
	if len(s.AllOf) != 0 {
		merged := map[string]any{}
		for _, child := range s.AllOf {
			if object, ok := schemaFixture(name, child).(map[string]any); ok {
				for key, value := range object {
					merged[key] = value
				}
			}
		}
		return merged
	}
	if s.Type != nil && s.Type.Is("object") || len(s.Properties) != 0 {
		out := map[string]any{}
		required := make(map[string]bool, len(s.Required))
		for _, field := range s.Required {
			required[field] = true
		}
		for field, property := range s.Properties {
			if required[field] {
				out[field] = schemaFixture(field, property)
			}
		}
		// PATCH schemas intentionally have no required fields. Supply one
		// writable field so the flow reaches persistence instead of stopping at
		// the empty-update guard.
		if len(out) == 0 {
			for field, property := range s.Properties {
				if property.Value != nil && !property.Value.ReadOnly {
					out[field] = schemaFixture(field, property)
					break
				}
			}
		}
		return out
	}
	if s.Type != nil && s.Type.Is("array") {
		count := int(math.Max(1, float64(s.MinItems)))
		out := make([]any, count)
		for i := range out {
			out[i] = schemaFixture(name, s.Items)
		}
		return out
	}
	if s.Type != nil && s.Type.Is("boolean") {
		return true
	}
	if s.Type != nil && (s.Type.Is("integer") || s.Type.Is("number")) {
		if s.Min != nil && *s.Min > 1 {
			return int(*s.Min)
		}
		return 1
	}

	lower := strings.ToLower(name)
	switch {
	case s.Format == "uuid", strings.Contains(lower, "uuid"):
		return fixtureUUID
	case s.Format == "date-time":
		return "2026-01-02T03:04:05Z"
	case s.Format == "email", strings.Contains(lower, "email"):
		return "unit@example.test"
	case s.Format == "uri", strings.Contains(lower, "url"), strings.Contains(lower, "endpoint"):
		return "https://example.test"
	case strings.Contains(lower, "password"):
		return "Unit-test-password-42!"
	case strings.Contains(lower, "cron"):
		return "0 * * * *"
	case strings.Contains(lower, "timezone"):
		return "Europe/Paris"
	case strings.Contains(lower, "repository"):
		return "https://github.com/acme/unit.git"
	case lower == "branch":
		return "main"
	case strings.Contains(lower, "compose"):
		return "services:\n  app:\n    image: nginx:1.27\n"
	case strings.Contains(lower, "domain"), strings.Contains(lower, "fqdn"):
		return "app.example.test"
	case lower == "target":
		return "https://app.example.test/health"
	case strings.Contains(lower, "port"):
		return "8080"
	case strings.Contains(lower, "path"):
		return "/data"
	}
	return strings.Repeat("u", int(max(uint64(1), s.MinLength)))
}

// TestContractOperationsReachTheirModuleFlow drives every authenticated
// OpenAPI operation through the generated decoder, real handler and real sqlc
// scanners. It is a module-level unit test: network, PostgreSQL and SSH are
// replaced, while request validation, RBAC, mapping, transaction ordering and
// response encoding remain real.
func TestContractOperationsReachTheirModuleFlow(t *testing.T) {
	a, _ := flowAPI(t)
	exerciseContractOperations(t, flowRouter(a))
}

func TestContractOperationsReachTruthyBranches(t *testing.T) {
	a, db := flowAPI(t)
	db.truthy = true
	exerciseContractOperations(t, flowRouter(a))
}

func TestContractOperationsReachPositiveCountBranches(t *testing.T) {
	a, db := flowAPI(t)
	db.truthy = true
	db.countOne = true
	exerciseContractOperations(t, flowRouter(a))
}

func TestContractOperationsHandleMissingRows(t *testing.T) {
	a, db := flowAPI(t)
	db.noRows = true
	exerciseContractOperations(t, flowRouter(a))
}

func exerciseContractOperations(t *testing.T, router http.Handler) {
	t.Helper()
	for _, flow := range flowOperations(t) {
		t.Run(flow.op.OperationID, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, requestForFlow(t, flow))
			if rec.Code >= http.StatusBadRequest {
				t.Logf("%s %s -> %d %s", flow.method, flow.path, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden ||
				rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "no matching operation") {
				t.Fatalf("%s %s did not reach its handler: %d %s", flow.method, flow.path, rec.Code, rec.Body.String())
			}
		})
	}
}

func flowRouter(a *API) http.Handler {
	root := &auth.Identity{
		TokenID: 1, TokenUUID: fixtureUUID, TeamID: 1, TeamUUID: fixtureUUID,
		Permissions: []string{string(auth.PermRoot)},
	}
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithIdentity(r.Context(), root)))
		})
	}
	return recoverJSON(a.Logger)(api.HandlerWithOptions(a, api.ChiServerOptions{
		BaseURL:     "/api/v1",
		Middlewares: []api.MiddlewareFunc{inject},
	}))
}

func TestContractOperationsHandleDatabaseFailure(t *testing.T) {
	a, db := flowAPI(t)
	db.err = errors.New("database unavailable")
	router := flowRouter(a)
	for _, flow := range flowOperations(t) {
		t.Run(flow.op.OperationID, func(t *testing.T) {
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, requestForFlow(t, flow))
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden ||
				rec.Code == http.StatusNotFound && strings.Contains(rec.Body.String(), "no matching operation") {
				t.Fatalf("%s %s skipped its handler while the store was failing: %d %s",
					flow.method, flow.path, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestFlowDBErrorsArePropagated(t *testing.T) {
	db := &flowDB{err: errors.New("database unavailable")}
	if _, err := db.Exec(context.Background(), "update"); err == nil {
		t.Fatal("Exec swallowed the configured error")
	}
	if _, err := db.Query(context.Background(), "select"); err == nil {
		t.Fatal("Query swallowed the configured error")
	}
	if err := db.QueryRow(context.Background(), "select").Scan(new(int64)); err == nil {
		t.Fatal("QueryRow swallowed the configured error")
	}
}

func TestFlowDBScansCompositeApplicationRow(t *testing.T) {
	db := &flowDB{}
	q := store.New(db)
	var u pgtype.UUID
	_ = u.Scan(fixtureUUID)
	if _, err := q.GetApplicationByUUID(context.Background(), store.GetApplicationByUUIDParams{
		Uuid: u, TeamID: 1,
	}); err != nil {
		t.Fatalf("composite application fixture: %v", err)
	}
}

func TestStreamEventsStopsWhenClientDisconnects(t *testing.T) {
	a, _ := flowAPI(t)
	ctx, cancel := context.WithCancel(auth.WithIdentity(context.Background(), &auth.Identity{
		TokenID: 1, TokenUUID: fixtureUUID, TeamID: 1, TeamUUID: fixtureUUID,
		Permissions: []string{string(auth.PermRead)},
	}))
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	a.StreamEvents(rec, req, api.StreamEventsParams{})

	if rec.Code != http.StatusOK {
		t.Fatalf("StreamEvents status = %d, want 200", rec.Code)
	}
}
