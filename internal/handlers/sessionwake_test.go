// ADR-067's wake half, both families. What is asserted here is mostly what
// must NOT happen — no command for a target with no clock, no session row
// behind a refusal, no operation attempted while a wake is in flight — because
// those are the clauses a future change silently breaks while every happy path
// keeps passing.
//
// Every top-level identifier is prefixed swake (concurrent-agent rule).
package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/audit"
	"github.com/deepteams/akerdock/internal/auth"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/terminal"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// swakeDB is the steerable fake with a statement log that keeps the ARGUMENTS.
// Most of this file asserts the absence of a write, and half of it asserts
// which audit action was written — neither is observable from a response body.
type swakeDB struct {
	*netcovDB
	mu    sync.Mutex
	calls []swakeCall
}

type swakeCall struct {
	sql  string
	args []any
}

func (db *swakeDB) record(sql string, args []any) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.calls = append(db.calls, swakeCall{sql: sql, args: args})
}

// ran reports whether a named sqlc query was executed at all.
func (db *swakeDB) ran(name string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, c := range db.calls {
		if strings.Contains(c.sql, "-- name: "+name+" ") {
			return true
		}
	}
	return false
}

// auditActions lists the actions of every audit row written so far.
func (db *swakeDB) auditActions() []string {
	db.mu.Lock()
	defer db.mu.Unlock()
	var out []string
	for _, c := range db.calls {
		if !strings.Contains(c.sql, "-- name: InsertAuditEvent ") {
			continue
		}
		for _, arg := range c.args {
			if s, ok := arg.(string); ok && strings.Contains(s, ".") {
				out = append(out, s)
				break
			}
		}
	}
	return out
}

func (db *swakeDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	db.record(sql, args)
	return db.netcovDB.Exec(ctx, sql, args...)
}

func (db *swakeDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	db.record(sql, args)
	return db.netcovDB.Query(ctx, sql, args...)
}

func (db *swakeDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	db.record(sql, args)
	return db.netcovDB.QueryRow(ctx, sql, args...)
}

var _ store.DBTX = (*swakeDB)(nil)

func swakeAPI(t *testing.T) (*API, *swakeDB) {
	t.Helper()
	a, inner := netcovAPI(t)
	db := &swakeDB{netcovDB: inner}
	a.Store = store.New(db)
	// The recorder keeps its own handle on the store, so pointing only a.Store
	// at the logging fake would make every audit assertion below vacuously
	// true — the rows would be written somewhere this test cannot see.
	a.Audit = &audit.Recorder{Store: a.Store, Logger: netcovLogger()}
	return a, db
}

// swakeAgent registers a scripted channel for server 1. Unlike netcovAgent the
// handler decides WHEN to answer, which is the whole point here: a wake in
// flight is a command with no result yet.
func swakeAgent(t *testing.T, a *API, handle func(cmd agentwire.Command, reply func(*agentwire.Result))) {
	t.Helper()
	ac, agent := dialPair(t)
	a.AgentRPC = &AgentConns{}
	a.AgentRPC.register(1, ac)
	var writeMu sync.Mutex
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for {
			_, data, err := agent.Read(ctx)
			if err != nil {
				return
			}
			var f agentwire.Frame
			if json.Unmarshal(data, &f) != nil || f.Type != agentwire.FrameCommand || f.Cmd == nil {
				continue
			}
			cmd := *f.Cmd
			reply := func(res *agentwire.Result) {
				if res == nil {
					return
				}
				res.ID = cmd.ID
				writeMu.Lock()
				defer writeMu.Unlock()
				_ = agentWrite(agent, agentwire.Frame{Type: agentwire.FrameResult, Res: res})
			}
			handle(cmd, reply)
		}
	}()
}

// swakeWakeCounter scripts the channel to answer WakeResource with `res` and
// counts how many wakes were asked for.
func swakeWakeCounter(t *testing.T, a *API, res *agentwire.Result) func() int {
	t.Helper()
	var mu sync.Mutex
	n := 0
	swakeAgent(t, a, func(cmd agentwire.Command, reply func(*agentwire.Result)) {
		if cmd.Method == agentwire.MethodWakeResource {
			mu.Lock()
			n++
			mu.Unlock()
			reply(res)
			return
		}
		reply(&agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected " + cmd.Method}})
	})
	return func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

func swakeUUID(t *testing.T) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(fixtureUUID); err != nil {
		t.Fatal(err)
	}
	return u
}

// swakeSleepingPreview is §8's first row: armed, asleep, parent running.
func swakeSleepingPreview(t *testing.T) sessionWakeSpec {
	t.Helper()
	id := int64(11)
	return sessionWakeSpec{
		kind: wakeKindPreview, serverID: 1, teamID: 1, uuid: swakeUUID(t),
		previewID: &id, armed: true, asleep: true, desiredRunning: true,
	}
}

// swakeSleepingApplication is §8's third row: armed, slept, desired running.
func swakeSleepingApplication(t *testing.T) sessionWakeSpec {
	t.Helper()
	id := int64(9)
	return sessionWakeSpec{
		kind: wakeKindApplication, serverID: 1, teamID: 1, uuid: swakeUUID(t),
		applicationID: &id, armed: true, asleep: true, desiredRunning: true,
	}
}

// swakeIdentity is a session-opening identity WITHOUT applications:lifecycle —
// the exact shape §7's refusal is about.
func swakeIdentity(perms ...auth.Permission) *auth.Identity {
	id := netcovIdentity()
	id.Permissions = nil
	for _, p := range perms {
		id.Permissions = append(id.Permissions, string(p))
	}
	return id
}

func swakeRequest(t *testing.T, id *auth.Identity) *http.Request {
	t.Helper()
	return netcovRequest(t, http.MethodPost, "/applications/x/port-forwards", nil, id)
}

// ---------------------------------------------------------------------------
// §8 — who is woken, and who is not
// ---------------------------------------------------------------------------

func TestSwakeVerdictFollowsTheTable(t *testing.T) {
	base := swakeSleepingApplication(t)
	preview := swakeSleepingPreview(t)

	cases := []struct {
		name  string
		spec  sessionWakeSpec
		want  wakeVerdict
		says  string
		nowak bool
	}{
		{name: "preview armed and sleeping", spec: preview, want: wakeAsk},
		{name: "application armed, slept, desired running", spec: base, want: wakeAsk},
		{
			name: "target with no clock at all",
			spec: sessionWakeSpec{}, want: wakeSkip,
		},
		{
			name: "scale-to-zero off",
			spec: func() sessionWakeSpec { s := base; s.armed = false; return s }(),
			want: wakeSkip,
		},
		{
			name: "already awake",
			spec: func() sessionWakeSpec { s := base; s.asleep = false; return s }(),
			want: wakeSkip,
		},
		{
			name: "manually stopped",
			spec: func() sessionWakeSpec { s := base; s.desiredRunning = false; return s }(),
			want: wakeRefuse, says: "start it",
		},
		{
			name: "manually stopped preview parent",
			spec: func() sessionWakeSpec { s := preview; s.desiredRunning = false; return s }(),
			want: wakeRefuse, says: "start it",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, msg := tc.spec.verdict()
			if got != tc.want {
				t.Fatalf("verdict = %v, want %v", got, tc.want)
			}
			if tc.says != "" && !strings.Contains(msg, tc.says) {
				t.Fatalf("message %q does not name the manual start", msg)
			}
			if tc.says == "" && msg != "" {
				t.Fatalf("unexpected message %q", msg)
			}
		})
	}
}

// The two builders are the only place the ADR's columns meet the schema's, and
// a mix-up there is invisible everywhere else: a preview keyed by the
// application's uuid would wake the wrong wake set, and reading the wrong flag
// would wake a resource whose operator armed nothing.
func TestSwakeSpecsReadTheRightColumns(t *testing.T) {
	appUUID, previewUUID := swakeUUID(t), swakeUUID(t)
	previewUUID.Bytes[0] ^= 0xff
	row := store.GetApplicationByUUIDRow{ServerRowID: 7}
	row.Resource.ID = 9
	row.Resource.TeamID = 3
	row.Resource.Uuid = appUUID
	row.Resource.DesiredStatus = store.ResourceDesiredStatusRunning
	row.Application.ScaleToZero = true
	row.Application.ScaleSleptAt = pgtype.Timestamptz{Time: time.Now(), Valid: true}
	row.Application.PreviewScaleToZero = false

	app := applicationWakeSpec(row)
	if app.kind != wakeKindApplication || !app.armed || !app.asleep || !app.desiredRunning {
		t.Fatalf("application spec = %+v", app)
	}
	if app.uuid != appUUID || app.applicationID == nil || *app.applicationID != 9 || app.previewID != nil {
		t.Fatalf("application spec names the wrong row: %+v", app)
	}
	if app.serverID != 7 || app.teamID != 3 {
		t.Fatalf("application spec placement = %+v", app)
	}

	preview := previewWakeSpec(row, store.Preview{ID: 11, Uuid: previewUUID, Status: store.PreviewStatusSleeping})
	if preview.kind != wakeKindPreview {
		t.Fatalf("preview spec kind = %v", preview.kind)
	}
	// The PREVIEW's flag, not the application's — they are separate opt-ins.
	if preview.armed {
		t.Fatal("preview spec read the application's scale_to_zero flag")
	}
	// The PREVIEW's uuid: its containers are named after it (INV-011), and so
	// is the wake set the agent deposited.
	if preview.uuid != previewUUID || preview.applicationID != nil {
		t.Fatalf("preview spec names the wrong row: %+v", preview)
	}
	if !preview.asleep {
		t.Fatal("a sleeping preview must read as asleep")
	}

	// `waking` is asleep too: a second mint against a wake in flight queues
	// behind the module's gate rather than being told the target is up.
	waking := previewWakeSpec(row, store.Preview{ID: 11, Uuid: previewUUID, Status: store.PreviewStatusWaking})
	if !waking.asleep {
		t.Fatal("a preview already waking must still be treated as not-up")
	}
	active := previewWakeSpec(row, store.Preview{ID: 11, Uuid: previewUUID, Status: store.PreviewStatusActive})
	if active.asleep {
		t.Fatal("an active preview must not read as asleep")
	}
}

// ---------------------------------------------------------------------------
// §3 / §7 — the mint asks, refuses, and leaves no row behind a refusal
// ---------------------------------------------------------------------------

func TestSwakeMintWakesASleepingPreview(t *testing.T) {
	a, db := swakeAPI(t)
	wakes := swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{"started":["c1","c2"]}`)})

	rec := httptest.NewRecorder()
	resource := int64(9)
	a.createPortForward(rec, swakeRequest(t, swakeIdentity(auth.PermPortForwardsOpen)), netcovIdentity(),
		portForwardSpec{
			serverID: 1, resourceID: &resource, previewID: ptr(int64(11)),
			name: "shop · PR #4", port: 5432, wake: swakeSleepingPreview(t),
		})
	netcovStatus(t, rec, http.StatusCreated)

	if got := wakes(); got != 1 {
		t.Fatalf("WakeResource sent %d times, want exactly 1", got)
	}
	if !db.ran("CreatePortForwardSession") {
		t.Fatal("the session row was not created")
	}
	// §1's ordering hazard: the mint stamps the clock itself, on the wake path
	// necessarily — the resource has just been started and would otherwise be a
	// candidate for the very next scheduler pass.
	if !db.ran("RecordPreviewActivity") {
		t.Fatal("the mint did not stamp the preview's activity clock")
	}
	swakeEventually(t, func() bool { return swakeHas(db.auditActions(), "port-forward.wake") },
		"port-forward.wake was never audited against the resource")
}

func TestSwakeMintSendsNoCommandForTargetsWithoutAClock(t *testing.T) {
	cases := []struct {
		name string
		spec sessionWakeSpec
	}{
		{name: "managed database, compose service, external endpoint", spec: sessionWakeSpec{}},
		{
			name: "scale-to-zero off",
			spec: func() sessionWakeSpec { s := swakeSleepingApplication(t); s.armed = false; return s }(),
		},
		{
			name: "already awake",
			spec: func() sessionWakeSpec { s := swakeSleepingApplication(t); s.asleep = false; return s }(),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, db := swakeAPI(t)
			wakes := swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{}`)})
			rec := httptest.NewRecorder()
			resource := int64(9)
			a.createPortForward(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
				portForwardSpec{serverID: 1, resourceID: &resource, name: "db", port: 5432, wake: tc.spec})
			netcovStatus(t, rec, http.StatusCreated)
			if got := wakes(); got != 0 {
				t.Fatalf("WakeResource sent %d times, want none", got)
			}
			if swakeHas(db.auditActions(), "port-forward.wake") {
				t.Fatal("a wake was audited for a target that was never woken")
			}
			switch tc.spec.kind {
			case wakeKindNone:
				if db.ran("RecordPreviewActivity") || db.ran("RecordApplicationActivity") {
					t.Fatal("a target with no scale-to-zero clock had activity written for it")
				}
			default:
				// §1 stamps on the ALREADY-AWAKE branch too: one write closes the
				// same 20-second window between the mint and the first beat.
				if !db.ran("RecordApplicationActivity") {
					t.Fatal("the already-awake branch did not stamp the activity clock")
				}
			}
		})
	}
}

// A server shell has no container, no resource and no clock, and it must reach
// no wake code at all — its terminal:root and step-up path is untouched by this
// decision.
func TestSwakeServerShellWakesNothingAndStampsNothing(t *testing.T) {
	a, db := swakeAPI(t)
	wakes := swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{}`)})
	rec := httptest.NewRecorder()
	a.createTerminalSession(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
		terminalTargetSpec{kind: store.TerminalTargetServer, serverID: 1, name: "srv-1"})
	netcovStatus(t, rec, http.StatusCreated)
	if got := wakes(); got != 0 {
		t.Fatalf("a server shell asked for %d wakes", got)
	}
	if db.ran("RecordPreviewActivity") || db.ran("RecordApplicationActivity") {
		t.Fatal("a server shell wrote an activity clock")
	}
}

// A terminal on one COMPONENT of a Compose-deployed application records against
// the application, which is where the flag and the clock live — and wakes the
// application, the component being inside the wake set.
func TestSwakeComponentTerminalStampsTheApplication(t *testing.T) {
	a, db := swakeAPI(t)
	wakes := swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{}`)})
	rec := httptest.NewRecorder()
	resource := int64(9)
	a.createTerminalSession(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
		terminalTargetSpec{
			kind: store.TerminalTargetContainer, serverID: 1, resourceID: &resource,
			name: "shop · worker", component: "worker", wake: swakeSleepingApplication(t),
		})
	netcovStatus(t, rec, http.StatusCreated)
	if got := wakes(); got != 1 {
		t.Fatalf("WakeResource sent %d times, want exactly 1", got)
	}
	if !db.ran("RecordApplicationActivity") {
		t.Fatal("a component terminal did not stamp the application's clock")
	}
	if db.ran("RecordPreviewActivity") {
		t.Fatal("a component terminal stamped a preview")
	}
	// The audit action is the family's own: one rule, two doors, and an operator
	// reading the application's history sees which door it was.
	swakeEventually(t, func() bool { return swakeHas(db.auditActions(), "terminal.wake") },
		"terminal.wake was never audited against the resource")
	if swakeHas(db.auditActions(), "port-forward.wake") {
		t.Fatal("a terminal wake was audited under the tunnel's action")
	}
}

func TestSwakeMintRefusesAManuallyStoppedResource(t *testing.T) {
	spec := swakeSleepingApplication(t)
	spec.desiredRunning = false

	t.Run("port-forward", func(t *testing.T) {
		a, db := swakeAPI(t)
		wakes := swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{}`)})
		rec := httptest.NewRecorder()
		resource := int64(9)
		a.createPortForward(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
			portForwardSpec{serverID: 1, resourceID: &resource, name: "shop", port: 5432, wake: spec})
		netcovStatus(t, rec, http.StatusConflict)
		if !strings.Contains(rec.Body.String(), "start it") {
			t.Fatalf("the refusal does not name the manual start: %s", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "container") {
			t.Fatalf("the refusal blames a missing container: %s", rec.Body.String())
		}
		if got := wakes(); got != 0 {
			t.Fatalf("a stopped resource was asked to wake %d times", got)
		}
		if db.ran("CreatePortForwardSession") {
			t.Fatal("a refused mint created a session row")
		}
	})

	t.Run("terminal", func(t *testing.T) {
		a, db := swakeAPI(t)
		wakes := swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{}`)})
		rec := httptest.NewRecorder()
		resource := int64(9)
		a.createTerminalSession(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
			terminalTargetSpec{
				kind: store.TerminalTargetContainer, serverID: 1, resourceID: &resource,
				name: "shop", wake: spec,
			})
		netcovStatus(t, rec, http.StatusConflict)
		if got := wakes(); got != 0 {
			t.Fatalf("a stopped resource was asked to wake %d times", got)
		}
		if db.ran("CreateTerminalSession") {
			t.Fatal("a refused mint created a session row")
		}
	})
}

// §7, asserted per handler because they are different handlers: the session's
// own permission is the whole gate on a preview, and never on an application.
func TestSwakeApplicationWakeNeedsLifecycle(t *testing.T) {
	t.Run("port-forward without lifecycle", func(t *testing.T) {
		a, db := swakeAPI(t)
		wakes := swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{}`)})
		rec := httptest.NewRecorder()
		id := swakeIdentity(auth.PermPortForwardsOpen)
		resource := int64(9)
		a.createPortForward(rec, swakeRequest(t, id), id,
			portForwardSpec{
				serverID: 1, resourceID: &resource, name: "shop", port: 5432,
				wake: swakeSleepingApplication(t),
			})
		netcovStatus(t, rec, http.StatusForbidden)
		if !strings.Contains(rec.Body.String(), string(auth.PermApplicationsLifecycle)) {
			t.Fatalf("the 403 does not name the missing permission: %s", rec.Body.String())
		}
		if db.ran("CreatePortForwardSession") {
			t.Fatal("a 403 created a session row")
		}
		if got := wakes(); got != 0 {
			t.Fatalf("an unauthorized mint asked for %d wakes", got)
		}
	})

	t.Run("terminal without lifecycle", func(t *testing.T) {
		a, db := swakeAPI(t)
		wakes := swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{}`)})
		rec := httptest.NewRecorder()
		id := swakeIdentity(auth.PermTerminalOpen)
		resource := int64(9)
		a.createTerminalSession(rec, swakeRequest(t, id), id,
			terminalTargetSpec{
				kind: store.TerminalTargetContainer, serverID: 1,
				resourceID: &resource, name: "shop", wake: swakeSleepingApplication(t),
			})
		netcovStatus(t, rec, http.StatusForbidden)
		if !strings.Contains(rec.Body.String(), string(auth.PermApplicationsLifecycle)) {
			t.Fatalf("the 403 does not name the missing permission: %s", rec.Body.String())
		}
		if db.ran("CreateTerminalSession") {
			t.Fatal("a 403 created a session row")
		}
		if got := wakes(); got != 0 {
			t.Fatalf("an unauthorized mint asked for %d wakes", got)
		}
	})

	t.Run("a preview needs only the session permission", func(t *testing.T) {
		a, _ := swakeAPI(t)
		wakes := swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{}`)})
		rec := httptest.NewRecorder()
		id := swakeIdentity(auth.PermPortForwardsOpen)
		resource := int64(9)
		a.createPortForward(rec, swakeRequest(t, id), id,
			portForwardSpec{
				serverID: 1, resourceID: &resource, previewID: ptr(int64(11)),
				name: "shop · PR #4", port: 5432, wake: swakeSleepingPreview(t),
			})
		netcovStatus(t, rec, http.StatusCreated)
		if got := wakes(); got != 1 {
			t.Fatalf("a preview wake was sent %d times, want 1", got)
		}
	})
}

// An agent that predates ADR-067 answers `unimplemented`, and the ADR is
// explicit that this is a refusal at the mint rather than a session that dies at
// its first stream.
func TestSwakeMintRefusesAnAgentThatCannotWake(t *testing.T) {
	cases := []struct {
		name string
		res  *agentwire.Result
		says string
	}{
		{
			name: "unimplemented",
			res:  &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeUnimplemented, Message: "unknown method"}},
			says: "too old",
		},
		{
			name: "no wake set for that uuid",
			res:  &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeNotFound, Message: "no route"}},
			says: "no wake set",
		},
		{
			name: "channel unavailable",
			res:  &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeUnavailable, Message: "reconnecting"}},
			says: "try again shortly",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, db := swakeAPI(t)
			swakeWakeCounter(t, a, tc.res)
			rec := httptest.NewRecorder()
			resource := int64(9)
			a.createPortForward(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
				portForwardSpec{
					serverID: 1, resourceID: &resource, previewID: ptr(int64(11)),
					name: "shop", port: 5432, wake: swakeSleepingPreview(t),
				})
			netcovStatus(t, rec, http.StatusConflict)
			if !strings.Contains(rec.Body.String(), tc.says) {
				t.Fatalf("refusal %q does not say %q", rec.Body.String(), tc.says)
			}
			if db.ran("CreatePortForwardSession") {
				t.Fatal("a refused wake still minted a session")
			}
			// §7 wants the RESULT in the trail, not only the attempt: a wake that
			// never came up must be legible without joining the session log.
			swakeEventually(t, func() bool { return swakeHas(db.auditActions(), "port-forward.wake") },
				"a failed wake was never audited")
		})
	}
}

// No channel at all is the same refusal, reached without a round trip.
func TestSwakeMintRefusesWhenNoChannelIsRegistered(t *testing.T) {
	a, db := swakeAPI(t)
	a.AgentRPC = &AgentConns{}
	rec := httptest.NewRecorder()
	resource := int64(9)
	a.createPortForward(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
		portForwardSpec{
			serverID: 1, resourceID: &resource, previewID: ptr(int64(11)),
			name: "shop", port: 5432, wake: swakeSleepingPreview(t),
		})
	netcovStatus(t, rec, http.StatusConflict)
	if !strings.Contains(rec.Body.String(), "agent_unavailable") {
		t.Fatalf("a transient refusal must say so: %s", rec.Body.String())
	}
	if db.ran("CreatePortForwardSession") {
		t.Fatal("a refused wake still minted a session")
	}
}

// Two mints against the same sleeping resource each ask once and each get a
// usable session: the de-duplication is the waker module's single-flight gate,
// not something the control plane may assume or re-implement.
func TestSwakeTwoMintsBothGetAUsableSession(t *testing.T) {
	a, _ := swakeAPI(t)
	wakes := swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{"started":["c1"]}`)})
	resource := int64(9)

	for _, family := range []string{"tunnel", "terminal"} {
		rec := httptest.NewRecorder()
		switch family {
		case "tunnel":
			a.createPortForward(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
				portForwardSpec{
					serverID: 1, resourceID: &resource, previewID: ptr(int64(11)),
					name: "shop", port: 5432, wake: swakeSleepingPreview(t),
				})
		default:
			a.createTerminalSession(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
				terminalTargetSpec{
					kind: store.TerminalTargetContainer, serverID: 1,
					resourceID: &resource, previewID: ptr(int64(11)), name: "shop",
					wake: swakeSleepingPreview(t),
				})
		}
		netcovStatus(t, rec, http.StatusCreated)
	}
	if got := wakes(); got != 2 {
		t.Fatalf("two mints asked %d times, want one command each", got)
	}
}

// ---------------------------------------------------------------------------
// §6 — the mint response states `waking`
// ---------------------------------------------------------------------------

// swakeSilentWake scripts a channel that ACCEPTS the wake and never answers it:
// a genuine cold start, still in flight when the mint has to answer.
func swakeSilentWake(t *testing.T, a *API) {
	t.Helper()
	swakeAgent(t, a, func(cmd agentwire.Command, reply func(*agentwire.Result)) {
		if cmd.Method == agentwire.MethodWakeResource {
			return
		}
		reply(&agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected " + cmd.Method}})
	})
}

// The developer must learn about a cold start from the MINT — before the local
// listener is announced, before the terminal window appears — and not from a
// control frame arriving once the session is already open and apparently frozen.
func TestSwakeMintStatesWaking(t *testing.T) {
	t.Run("port-forward", func(t *testing.T) {
		a, _ := swakeAPI(t)
		swakeSilentWake(t, a)
		rec := httptest.NewRecorder()
		resource := int64(9)
		a.createPortForward(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
			portForwardSpec{
				serverID: 1, resourceID: &resource, previewID: ptr(int64(11)),
				name: "shop", port: 5432, wake: swakeSleepingPreview(t),
			})
		netcovStatus(t, rec, http.StatusCreated)

		var got api.PortForwardSession
		swakeDecode(t, rec, &got)
		if got.State == nil || *got.State != api.PortForwardSessionStateWaking {
			t.Fatalf("state = %v, want waking", got.State)
		}
		// And the session it announced is the one the attach will await.
		if a.lookupWake(got.Uuid) == nil {
			t.Fatal("a session announced as waking has no wake to await")
		}
	})

	t.Run("terminal", func(t *testing.T) {
		a, _ := swakeAPI(t)
		swakeSilentWake(t, a)
		rec := httptest.NewRecorder()
		resource := int64(9)
		a.createTerminalSession(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
			terminalTargetSpec{
				kind: store.TerminalTargetContainer, serverID: 1,
				resourceID: &resource, previewID: ptr(int64(11)), name: "shop",
				wake: swakeSleepingPreview(t),
			})
		netcovStatus(t, rec, http.StatusCreated)

		var got api.TerminalSession
		swakeDecode(t, rec, &got)
		if got.State == nil || *got.State != api.Waking {
			t.Fatalf("state = %v, want waking", got.State)
		}
		if a.lookupWake(got.Uuid) == nil {
			t.Fatal("a session announced as waking has no wake to await")
		}
	})
}

// Absent means ready, and it has to mean ready for two unrelated reasons at
// once: nothing was asleep, or the wake was already over before the mint
// answered. Neither is a cold start the developer should be warned about, and
// an older client that ignores the field lands on the same behaviour.
func TestSwakeMintOmitsStateWhenThereIsNothingToWaitFor(t *testing.T) {
	cases := []struct {
		name  string
		spec  sessionWakeSpec
		agent func(t *testing.T, a *API)
	}{
		{
			name:  "nothing was asleep",
			spec:  func() sessionWakeSpec { s := swakeSleepingPreview(t); s.asleep = false; return s }(),
			agent: func(t *testing.T, a *API) { swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{}`)}) },
		},
		{
			name: "the wake was over before the mint answered",
			spec: swakeSleepingPreview(t),
			agent: func(t *testing.T, a *API) {
				swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{"started":["c1"]}`)})
			},
		},
		{
			name:  "the target has no clock at all",
			spec:  sessionWakeSpec{},
			agent: func(t *testing.T, a *API) { swakeWakeCounter(t, a, &agentwire.Result{Body: []byte(`{}`)}) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := swakeAPI(t)
			tc.agent(t, a)
			rec := httptest.NewRecorder()
			resource := int64(9)
			a.createPortForward(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
				portForwardSpec{
					serverID: 1, resourceID: &resource, previewID: ptr(int64(11)),
					name: "shop", port: 5432, wake: tc.spec,
				})
			netcovStatus(t, rec, http.StatusCreated)
			var got api.PortForwardSession
			swakeDecode(t, rec, &got)
			if got.State != nil {
				t.Fatalf("state = %q, want the field absent", *got.State)
			}
			if !strings.Contains(rec.Body.String(), `"uuid"`) {
				t.Fatal("the mint answered nothing usable")
			}
			if strings.Contains(rec.Body.String(), `"state"`) {
				t.Fatalf("the field was serialized anyway: %s", rec.Body.String())
			}
		})
	}
}

// `waking` says what the mint ASKED FOR, not how it turned out, so a session
// announced as waking and then closed with wake_failed is one coherent story
// and not a contradiction. The refusal window is what keeps it that way in the
// other direction: a wake whose verdict is already in when the mint would
// answer is refused outright, and no session is announced for it at all.
func TestSwakeWakingThenWakeFailedIsNotAContradiction(t *testing.T) {
	a, db := swakeAPI(t)
	answer := make(chan *agentwire.Result, 1)
	swakeAgent(t, a, func(cmd agentwire.Command, reply func(*agentwire.Result)) {
		if cmd.Method == agentwire.MethodWakeResource {
			go func() { reply(<-answer) }()
			return
		}
		reply(&agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected"}})
	})

	rec := httptest.NewRecorder()
	resource := int64(9)
	a.createPortForward(rec, swakeRequest(t, netcovIdentity()), netcovIdentity(),
		portForwardSpec{
			serverID: 1, resourceID: &resource, previewID: ptr(int64(11)),
			name: "shop", port: 5432, wake: swakeSleepingPreview(t),
		})
	netcovStatus(t, rec, http.StatusCreated)
	var minted api.PortForwardSession
	swakeDecode(t, rec, &minted)
	if minted.State == nil || *minted.State != api.PortForwardSessionStateWaking {
		t.Fatalf("state = %v, want waking", minted.State)
	}

	// The wake fails only AFTER the mint has committed — the ordinary case, and
	// the only one that can produce this sequence.
	answer <- &agentwire.Result{Err: &agentwire.Error{
		Code: agentwire.CodeInternal, Message: "wake stalled on shop-postgres",
	}}
	wake := a.lookupWake(minted.Uuid)
	if wake == nil {
		t.Fatal("the announced wake is not findable")
	}
	msg, ok := wake.await(context.Background())
	if ok {
		t.Fatal("the wake reported ready")
	}
	if msg != "wake stalled on shop-postgres" {
		t.Fatalf("message = %q, want the waker's own text", msg)
	}
	if got := sessionEndReason(wake); got != endReasonWakeFailed {
		t.Fatalf("end reason = %q, want wake_failed after an announced waking", got)
	}
	swakeEventually(t, func() bool { return swakeHas(db.auditActions(), "port-forward.wake") },
		"the failed wake was never audited")
}

// ---------------------------------------------------------------------------
// §5 — the two gates
// ---------------------------------------------------------------------------

// The session's own operation must not be attempted while the wake is in
// flight: exec-ing into a container that has not been started yet is the exact
// failure this decision exists to remove, and doing it early would also mean the
// retry budget was already burning before the containers existed.
func TestSwakeGateOneHoldsTheOperation(t *testing.T) {
	a, _ := swakeAPI(t)
	execs := make(chan struct{}, 4)
	swakeAgent(t, a, func(cmd agentwire.Command, reply func(*agentwire.Result)) {
		switch cmd.Method {
		case agentwire.MethodContainerExecCreate:
			execs <- struct{}{}
			reply(&agentwire.Result{Body: []byte(`{"Id":"exec-1"}`)})
		case agentwire.MethodContainerExecAttach:
			reply(&agentwire.Result{})
		default:
			reply(&agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected"}})
		}
	})

	wake := &sessionWake{done: make(chan struct{})}
	opened := make(chan string, 1)
	go func() {
		pty, cleanup, msg := a.terminalOpenPTYAfterWake(context.Background(), wake,
			terminalTarget{server: store.Server{ID: 1}, container: "c1", cols: 80, rows: 24})
		if pty != nil {
			_ = pty.Close()
		}
		if cleanup != nil {
			cleanup()
		}
		opened <- msg
	}()

	select {
	case <-execs:
		t.Fatal("the exec was attempted while the wake was still in flight")
	case <-time.After(100 * time.Millisecond):
	}

	wake.settle([]string{"c1"}, nil)
	select {
	case <-execs:
	case <-time.After(3 * time.Second):
		t.Fatal("the exec never ran after the wake returned ready")
	}
	select {
	case msg := <-opened:
		if msg != "" {
			t.Fatalf("the shell was refused after a successful wake: %s", msg)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the resolver never returned")
	}
}

// Gate 1's verdict IS the command's answer: an error means the target did not
// come up, and the message the developer reads is the waker's own — it names
// the container the wake stalled on, and nothing here could reconstruct that.
func TestSwakeGateOneFailureCarriesTheWakerSentence(t *testing.T) {
	stalled := &agentwire.Error{
		Code:    agentwire.CodeInternal,
		Message: "wake stalled on container 6d50a89d-postgres (never became healthy)",
	}
	wake := &sessionWake{done: make(chan struct{})}
	wake.settle(nil, stalled.Err())

	msg, ok := wake.await(context.Background())
	if ok {
		t.Fatal("a failed wake reported ready")
	}
	if msg != stalled.Message {
		t.Fatalf("message = %q, want the waker's own text verbatim", msg)
	}
	if !wake.failedGate() {
		t.Fatal("a gate-1 failure must mark the session's end reason")
	}
	if got := sessionEndReason(wake); got != endReasonWakeFailed {
		t.Fatalf("end reason = %q, want wake_failed", got)
	}
	if got := terminalSessionEndReason(wake); got != terminal.EndReason("wake_failed") {
		t.Fatalf("terminal end reason = %q, want wake_failed", got)
	}
}

// A session that never had a wake, and one whose gates both passed, must NOT
// end as wake_failed: that value would send a developer looking at
// scale-to-zero for an SSH refusal.
func TestSwakeUnwokenSessionsKeepTargetUnreachable(t *testing.T) {
	if got := sessionEndReason(nil); got != endReasonTargetUnreachable {
		t.Fatalf("end reason without a wake = %q", got)
	}
	passed := &sessionWake{done: make(chan struct{})}
	passed.settle([]string{"c1"}, nil)
	if _, ok := passed.await(context.Background()); !ok {
		t.Fatal("a successful wake must report ready")
	}
	if got := sessionEndReason(passed); got != endReasonTargetUnreachable {
		t.Fatalf("end reason after a successful wake = %q", got)
	}
	if got := terminalSessionEndReason(nil); got != terminal.EndTargetUnreachable {
		t.Fatalf("terminal end reason without a wake = %q", got)
	}
}

// A session abandoned mid-wake reports nothing and finalizes nothing: there is
// nobody left to tell.
func TestSwakeAbandonedSessionSaysNothing(t *testing.T) {
	wake := &sessionWake{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg, ok := wake.await(ctx)
	if ok || msg != "" {
		t.Fatalf("await on an abandoned session = (%q, %v), want a silent refusal", msg, ok)
	}
	if wake.failedGate() {
		t.Fatal("an abandonment is not a wake failure")
	}
}

// Gate 2 is the real operation, retried with backoff — not a synthetic probe
// and not a single attempt.
func TestSwakeGateTwoRetriesUntilTheOperationSucceeds(t *testing.T) {
	attempts := 0
	start := time.Now()
	msg := retryUntilReady(context.Background(), func(context.Context) string {
		attempts++
		if attempts < 3 {
			return "connection refused"
		}
		return ""
	})
	if msg != "" {
		t.Fatalf("gate 2 gave up with %q", msg)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	// 250 ms then 500 ms: a retry loop that spins would pass the count and fail
	// this, which is the point of asserting it.
	if elapsed := time.Since(start); elapsed < sessionWakeBackoffFirst {
		t.Fatalf("gate 2 did not back off (elapsed %s)", elapsed)
	}
}

// Gate 2 ending without success is the same event as gate 1 ending without
// success, and the last failure is what the developer reads.
func TestSwakeGateTwoGivesUpWithTheLastFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	msg := retryUntilReady(ctx, func(context.Context) string { return "the port never accepted" })
	if msg != "the port never accepted" {
		t.Fatalf("gate 2 verdict = %q", msg)
	}
}

// The exec attach IS the terminal's gate 2, and it is retried: right after a
// start the daemon may still refuse one.
func TestSwakeTerminalGateTwoRetriesTheExec(t *testing.T) {
	a, _ := swakeAPI(t)
	var mu sync.Mutex
	creates := 0
	swakeAgent(t, a, func(cmd agentwire.Command, reply func(*agentwire.Result)) {
		switch cmd.Method {
		case agentwire.MethodContainerExecCreate:
			mu.Lock()
			creates++
			n := creates
			mu.Unlock()
			if n < 2 {
				reply(&agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeConflict, Message: "not running"}})
				return
			}
			reply(&agentwire.Result{Body: []byte(`{"Id":"exec-1"}`)})
		case agentwire.MethodContainerExecAttach:
			reply(&agentwire.Result{})
		default:
			reply(&agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected"}})
		}
	})

	wake := &sessionWake{done: make(chan struct{})}
	wake.settle(nil, nil)
	pty, cleanup, msg := a.terminalOpenPTYAfterWake(context.Background(), wake,
		terminalTarget{server: store.Server{ID: 1}, container: "c1", cols: 80, rows: 24})
	if msg != "" {
		t.Fatalf("gate 2 refused a shell that came up on the second try: %s", msg)
	}
	if pty == nil {
		t.Fatal("no PTY returned")
	}
	_ = pty.Close()
	if cleanup != nil {
		cleanup()
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 2 {
		t.Fatalf("exec created %d times, want a retry", creates)
	}
}

// A server shell and any target with no wake take the unchanged path: no gate,
// no retry, and the resolver called exactly once.
func TestSwakeNoWakeMeansNoGates(t *testing.T) {
	a, _ := swakeAPI(t)
	var mu sync.Mutex
	creates := 0
	swakeAgent(t, a, func(cmd agentwire.Command, reply func(*agentwire.Result)) {
		if cmd.Method == agentwire.MethodContainerExecCreate {
			mu.Lock()
			creates++
			mu.Unlock()
			reply(&agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeConflict, Message: "not running"}})
			return
		}
		reply(&agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected"}})
	})
	_, _, msg := a.terminalOpenPTYAfterWake(context.Background(), nil,
		terminalTarget{server: store.Server{ID: 1}, container: "c1", cols: 80, rows: 24})
	if msg == "" {
		t.Fatal("a refused exec must still be refused when there is no wake")
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 {
		t.Fatalf("exec attempted %d times without a wake, want exactly 1", creates)
	}
}

// ---------------------------------------------------------------------------
// §6 — what the developer is told
// ---------------------------------------------------------------------------

func TestSwakeAnnouncesTheColdStartAndThenReady(t *testing.T) {
	wake := &sessionWake{done: make(chan struct{})}
	frames := swakeControl(t)
	announceWake(context.Background(), frames.control, wake)

	first := frames.next(t)
	if first.Type != wakeControlFrame || first.Code != wakeFrameColdStart {
		t.Fatalf("first frame = %+v, want a cold-start notice", first)
	}
	if !strings.Contains(first.Msg, "scale-to-zero") || !strings.Contains(first.Msg, "75") {
		t.Fatalf("the notice must name scale-to-zero and the ceiling: %q", first.Msg)
	}
	wake.settle(nil, nil)
	second := frames.next(t)
	if second.Type != wakeControlFrame || second.Code != wakeFrameReady {
		t.Fatalf("second frame = %+v, want the ready notice", second)
	}
}

// A wake that FAILS gets no ready frame: the session's close carries the
// verdict, and a "ready" the developer never got would be a lie on the wire.
func TestSwakeAnnouncesNoReadyWhenTheWakeFails(t *testing.T) {
	wake := &sessionWake{done: make(chan struct{})}
	frames := swakeControl(t)
	announceWake(context.Background(), frames.control, wake)
	_ = frames.next(t)
	wake.settle(nil, errors.New("agent: stalled"))
	if got, ok := frames.tryNext(200 * time.Millisecond); ok {
		t.Fatalf("a failed wake announced %+v", got)
	}
}

// A session that never woke anything says nothing at all.
func TestSwakeAnnouncesNothingWithoutAWake(t *testing.T) {
	frames := swakeControl(t)
	announceWake(context.Background(), frames.control, nil)
	if got, ok := frames.tryNext(100 * time.Millisecond); ok {
		t.Fatalf("a session with no wake announced %+v", got)
	}
	settled := &sessionWake{done: make(chan struct{})}
	settled.settle(nil, nil)
	announceWake(context.Background(), frames.control, settled)
	if got, ok := frames.tryNext(100 * time.Millisecond); ok {
		t.Fatalf("an already-ready wake announced %+v", got)
	}
}

// The first connection is supposed to WAIT for the cold start; the ordinary
// dial budget would refuse it while the platform did what it announced.
func TestSwakeStreamBudgetWidensOnlyForAWake(t *testing.T) {
	if got := wakeStreamBudget(nil); got != tunnel.EgressDialTimeout {
		t.Fatalf("budget without a wake = %s", got)
	}
	got := wakeStreamBudget(&sessionWake{done: make(chan struct{})})
	if got < sessionWakeCeiling {
		t.Fatalf("budget with a wake = %s, shorter than the ceiling it must outlast", got)
	}
	if sessionWakeCeiling != sessionWakeGate1+sessionWakeGate2 {
		t.Fatal("the ceiling must stay the sum of the two gates, never a third budget")
	}
	if sessionWakeCeiling != 75*time.Second {
		t.Fatalf("the ceiling is %s — ADR-067 §5 states 75 s", sessionWakeCeiling)
	}
}

// Each typed answer gets the phrasing its remedy needs; only the internal class
// passes the waker's own words through.
func TestSwakeFailureMessages(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "none", err: nil, want: ""},
		{
			name: "unimplemented",
			err:  (&agentwire.Error{Code: agentwire.CodeUnimplemented, Message: "x"}).Err(),
			want: "too old",
		},
		{
			name: "not found",
			err:  (&agentwire.Error{Code: agentwire.CodeNotFound, Message: "x"}).Err(),
			want: "no wake set",
		},
		{
			name: "unavailable",
			err:  (&agentwire.Error{Code: agentwire.CodeUnavailable, Message: "x"}).Err(),
			want: "try again shortly",
		},
		{
			name: "our own ceiling",
			err:  context.DeadlineExceeded,
			want: "rolled back",
		},
		{
			name: "invalid",
			err:  cerrdefs.ErrInvalidArgument,
			want: "bug in AkerDock",
		},
		{
			name: "internal is the waker's own sentence",
			err:  (&agentwire.Error{Code: agentwire.CodeInternal, Message: "stalled on shop-postgres"}).Err(),
			want: "stalled on shop-postgres",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wakeFailureMessage(tc.err)
			if tc.want == "" {
				if got != "" {
					t.Fatalf("message = %q, want none", got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("message = %q, want it to contain %q", got, tc.want)
			}
			if strings.HasPrefix(got, "agent:") {
				t.Fatalf("the channel's own prefix reached the developer: %q", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Registry
// ---------------------------------------------------------------------------

func TestSwakeRegistryIsPerSession(t *testing.T) {
	a, _ := swakeAPI(t)
	if got := a.lookupWake("nobody"); got != nil {
		t.Fatal("an unknown session found a wake")
	}
	wake := &sessionWake{done: make(chan struct{})}
	a.rememberWake("s-1", wake)
	if got := a.lookupWake("s-1"); got != wake {
		t.Fatal("the wake was not findable by the session it was asked for")
	}
	if got := a.lookupWake("s-2"); got != nil {
		t.Fatal("a wake leaked to another session")
	}
	a.forgetWake("s-1", wake)
	if got := a.lookupWake("s-1"); got != nil {
		t.Fatal("a forgotten wake is still findable")
	}
	// Registering nil is what a session with nothing to wake does, and it must
	// not create an entry a later attach would wait on.
	a.rememberWake("s-3", nil)
	if got := a.lookupWake("s-3"); got != nil {
		t.Fatal("a session with no wake got a registry entry")
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// swakeControlWire is one end of a LineControl whose frames a test can read.
type swakeControlWire struct {
	control *tunnel.LineControl
	frames  chan tunnel.HTTPControlFrame
}

func swakeControl(t *testing.T) *swakeControlWire {
	t.Helper()
	server, client := swakePipe(t)
	wire := &swakeControlWire{
		control: tunnel.NewLineControl(strings.NewReader(""), server, nil, server.Close),
		frames:  make(chan tunnel.HTTPControlFrame, 8),
	}
	reader := tunnel.NewLineControl(client, swakeDiscard{}, nil, client.Close)
	go func() {
		for {
			frame, err := reader.Receive()
			if err != nil {
				close(wire.frames)
				return
			}
			wire.frames <- frame
		}
	}()
	return wire
}

func (w *swakeControlWire) next(t *testing.T) tunnel.HTTPControlFrame {
	t.Helper()
	frame, ok := w.tryNext(2 * time.Second)
	if !ok {
		t.Fatal("no control frame arrived")
	}
	return frame
}

func (w *swakeControlWire) tryNext(within time.Duration) (tunnel.HTTPControlFrame, bool) {
	select {
	case frame, ok := <-w.frames:
		return frame, ok
	case <-time.After(within):
		return tunnel.HTTPControlFrame{}, false
	}
}

type swakeDiscard struct{}

func (swakeDiscard) Write(p []byte) (int, error) { return len(p), nil }

// swakePipe is an in-memory full-duplex pair, closed with the test.
func swakePipe(t *testing.T) (server, client *swakeConn) {
	t.Helper()
	toClient := make(chan []byte, 32)
	server = &swakeConn{out: toClient}
	client = &swakeConn{in: toClient}
	t.Cleanup(func() {
		_ = server.Close()
	})
	return server, client
}

type swakeConn struct {
	out       chan []byte
	in        chan []byte
	rest      []byte
	closeOnce sync.Once
}

func (c *swakeConn) Write(p []byte) (int, error) {
	buf := make([]byte, len(p))
	copy(buf, p)
	c.out <- buf
	return len(p), nil
}

func (c *swakeConn) Read(p []byte) (int, error) {
	for len(c.rest) == 0 {
		chunk, ok := <-c.in
		if !ok {
			return 0, errors.New("closed")
		}
		c.rest = chunk
	}
	n := copy(p, c.rest)
	c.rest = c.rest[n:]
	return n, nil
}

func (c *swakeConn) Close() error {
	c.closeOnce.Do(func() {
		if c.out != nil {
			close(c.out)
		}
	})
	return nil
}

func swakeDecode(t *testing.T, rec *httptest.ResponseRecorder, into any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), into); err != nil {
		t.Fatalf("decode mint response %s: %v", rec.Body.String(), err)
	}
}

func swakeHas(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

// swakeEventually polls for a fact produced by the wake's own goroutine — the
// audit row is written after the command settles, not before the mint answers.
func swakeEventually(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal(msg)
}
