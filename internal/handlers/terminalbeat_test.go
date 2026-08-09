// The terminal heartbeat: the 20-second beat both rungs run (ADR-024 over
// ADR-064), and what ADR-067 §1 and §2 hang off it. It is the only moment an
// attached shell talks to the control plane while a developer reads a log, so
// two otherwise unrelated duties ride on it — telling scale-to-zero somebody is
// connected, and noticing that the target container is gone.
//
// The tunnel's twin is portforwardbeat_test.go, and the resemblance is the
// point: one rule, two bridges. The last test here is the one that would catch
// them drifting apart.
//
// Every top-level identifier is prefixed tbeat (concurrent-agent rule).
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

	"github.com/coder/websocket"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/terminal"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// tbeatDB wraps the steerable netcov fake with a call log. What a beat does is
// CHOOSE a statement — the preview one, the application one, or none at all —
// and those statements return nothing a response could show, so the log is the
// only place that choice is observable.
type tbeatDB struct {
	*netcovDB
	mu    sync.Mutex
	calls []string
}

func (db *tbeatDB) record(sql string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.calls = append(db.calls, sql)
}

func (db *tbeatDB) ran(name string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, sql := range db.calls {
		if strings.Contains(sql, "-- name: "+name+" ") {
			return true
		}
	}
	return false
}

func (db *tbeatDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	db.record(sql)
	return db.netcovDB.Exec(ctx, sql, args...)
}

func (db *tbeatDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	db.record(sql)
	return db.netcovDB.Query(ctx, sql, args...)
}

func (db *tbeatDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	db.record(sql)
	return db.netcovDB.QueryRow(ctx, sql, args...)
}

var _ store.DBTX = (*tbeatDB)(nil)

func tbeatAPI(t *testing.T) (*API, *tbeatDB) {
	t.Helper()
	a, inner := netcovAPI(t)
	db := &tbeatDB{netcovDB: inner}
	a.Store = store.New(db)
	return a, db
}

// tbeatSession is a claimed container shell on server 1 — the server
// netcovAgent registers its scripted channel for.
func tbeatSession() store.TerminalSession {
	server, resource := int64(1), int64(9)
	row := store.TerminalSession{
		ID: 42, TeamID: 1, AttachSeq: 1, TargetKind: store.TerminalTargetContainer,
		ServerID: &server, ResourceID: &resource, TargetName: "shop",
	}
	_ = row.Uuid.Scan(fixtureUUID)
	return row
}

func tbeatPreviewSession() store.TerminalSession {
	row := tbeatSession()
	preview := int64(11)
	row.PreviewID = &preview
	return row
}

// tbeatServerShell is the session this whole decision excludes: no container,
// no resource, no scale-to-zero clock (ADR-067's scope paragraph).
func tbeatServerShell() store.TerminalSession {
	row := tbeatSession()
	row.TargetKind = store.TerminalTargetServer
	row.ResourceID = nil
	return row
}

// tbeatInspect scripts the agent channel's ContainerInspect answer.
func tbeatInspect(t *testing.T, a *API, body string, failure *agentwire.Error) {
	t.Helper()
	netcovAgent(t, a, func(cmd agentwire.Command) (*agentwire.Result, bool) {
		if cmd.Method != agentwire.MethodContainerInspect {
			return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected"}}, false
		}
		if failure != nil {
			return &agentwire.Result{Err: failure}, false
		}
		return &agentwire.Result{Body: []byte(body)}, false
	})
}

// tbeatRun beats once and reports both of the beat's outputs: the reason it
// ENDED the session with, empty while the session lives, and the reason it CUT
// the session with through the attach register a re-claim uses. Two channels on
// purpose — a container that vanished is cut here and now, while a row finalized
// on another replica is already over and the beat carries its word back to the
// bridge directly.
func tbeatRun(t *testing.T, a *API, row store.TerminalSession) (ended, cut terminal.EndReason) {
	t.Helper()
	attach := newTerminalAttach([32]byte{}, nil)
	a.terminalRegister(uuidString(row.Uuid), attach)
	defer a.terminalRelease(uuidString(row.Uuid), attach)
	ended = a.terminalHeartbeat(row)(context.Background())
	select {
	case reason := <-attach.cut:
		return ended, reason
	default:
		return ended, ""
	}
}

func tbeatEventually(t *testing.T, condition func() bool, complaint string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(complaint)
}

// An attached shell is activity for the resource it is attached to — the signal
// the waker cannot produce, since a terminal crosses the agent as an opaque
// exec attach on the command channel and never as something the waker module
// can attribute to a resource's clock.
func TestTbeatHeartbeatRecordsActivityForItsTarget(t *testing.T) {
	cases := map[string]struct {
		row     store.TerminalSession
		arrange func(db *tbeatDB)
		preview bool
		app     bool
	}{
		"preview":     {row: tbeatPreviewSession(), preview: true},
		"application": {row: tbeatSession(), app: true},
		// A managed database has no scale-to-zero at all (ADR-037 §2 excludes
		// them by construction): there is no clock to reset, and writing one
		// would be inventing its semantics with it.
		"database": {
			row: tbeatSession(),
			arrange: func(db *tbeatDB) {
				db.rule(netcovRule{match: "-- name: GetResourceByID ", typed: []any{store.ResourceTypeDatabase}})
			},
		},
		// A Compose *service* resource has no scale-to-zero flag either.
		"compose service": {
			row: tbeatSession(),
			arrange: func(db *tbeatDB) {
				db.rule(netcovRule{match: "-- name: GetResourceByID ", typed: []any{store.ResourceTypeService}})
			},
		},
		// A server shell has no container and no resource: it is outside this
		// decision in both directions.
		"server shell": {row: tbeatServerShell()},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a, db := tbeatAPI(t)
			if tc.arrange != nil {
				tc.arrange(db)
			}
			ended, reason := tbeatRun(t, a, tc.row)
			if ended != "" || reason != "" {
				t.Fatalf("beat ended the session (ended=%q cut=%q) — nothing here is a reason to", ended, reason)
			}
			if got := db.ran("RecordPreviewActivity"); got != tc.preview {
				t.Errorf("preview activity recorded = %v, want %v", got, tc.preview)
			}
			if got := db.ran("RecordApplicationActivity"); got != tc.app {
				t.Errorf("application activity recorded = %v, want %v", got, tc.app)
			}
			if !db.ran("HeartbeatTerminalSession") {
				t.Error("liveness must still be persisted: it is the crash/restart net")
			}
		})
	}
}

// A terminal opened on one COMPONENT of a Compose-deployed application records
// against the application, which is where the flag and the clock live.
func TestTbeatComponentBeatStampsTheApplication(t *testing.T) {
	a, db := tbeatAPI(t)
	row := tbeatSession()
	row.TargetComponent = ptr("worker")
	if ended, reason := tbeatRun(t, a, row); ended != "" || reason != "" {
		t.Fatalf("ended=%q cut=%q", ended, reason)
	}
	if !db.ran("RecordApplicationActivity") {
		t.Fatal("a component shell did not stamp the application's clock")
	}
	if db.ran("RecordPreviewActivity") {
		t.Fatal("a component shell stamped a preview")
	}
}

// The window the first beat cannot cover (ADR-067 §1): a shell is opened
// precisely because nothing has touched the resource lately, so the mint races
// the sleep decision at 29:50 of a 30-minute window — and a stamp that waits
// twenty seconds for the first heartbeat arrives after the containers stopped.
func TestTbeatMintStampsActivityForItsTarget(t *testing.T) {
	cases := map[string]struct {
		mint    func(a *API, rec *httptest.ResponseRecorder, r *http.Request)
		preview bool
		app     bool
	}{
		"application": {
			mint: func(a *API, rec *httptest.ResponseRecorder, r *http.Request) {
				a.CreateApplicationTerminalSession(rec, r, fixtureUUID, api.CreateApplicationTerminalSessionParams{})
			},
			app: true,
		},
		"preview": {
			mint: func(a *API, rec *httptest.ResponseRecorder, r *http.Request) {
				a.CreatePreviewTerminalSession(rec, r, fixtureUUID, fixtureUUID, api.CreatePreviewTerminalSessionParams{})
			},
			preview: true,
		},
		"database": {
			mint: func(a *API, rec *httptest.ResponseRecorder, r *http.Request) {
				a.CreateDatabaseTerminalSession(rec, r, fixtureUUID)
			},
		},
		// terminal:root and its step-up path reach no part of this decision.
		"server shell": {
			mint: func(a *API, rec *httptest.ResponseRecorder, r *http.Request) {
				a.CreateServerTerminalSession(rec, r, fixtureUUID)
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a, db := tbeatAPI(t)
			rec := httptest.NewRecorder()
			tc.mint(a, rec, netcovRequest(t, http.MethodPost, "/terminal-sessions", nil, netcovIdentity()))
			netcovStatus(t, rec, http.StatusCreated)
			if got := db.ran("RecordPreviewActivity"); got != tc.preview {
				t.Errorf("preview activity stamped at mint = %v, want %v", got, tc.preview)
			}
			if got := db.ran("RecordApplicationActivity"); got != tc.app {
				t.Errorf("application activity stamped at mint = %v, want %v", got, tc.app)
			}
		})
	}
	// A timestamp is never a reason to refuse a shell.
	t.Run("a failed stamp does not fail the mint", func(t *testing.T) {
		a, db := tbeatAPI(t)
		db.rule(netcovRule{match: "-- name: RecordApplicationActivity ", err: errors.New("connection reset")})
		rec := httptest.NewRecorder()
		a.CreateApplicationTerminalSession(rec, netcovRequest(t, http.MethodPost, "/terminal-sessions", nil, netcovIdentity()),
			fixtureUUID, api.CreateApplicationTerminalSessionParams{})
		netcovStatus(t, rec, http.StatusCreated)
	})
}

// The beat's contract on failure: the socket is the source of truth while this
// process is alive, so a database that blinks is logged and retried 20 seconds
// later — never a reason to drop a working shell.
func TestTbeatHeartbeatSurvivesADatabaseFailure(t *testing.T) {
	t.Run("liveness update fails", func(t *testing.T) {
		a, db := tbeatAPI(t)
		db.rule(netcovRule{match: "-- name: HeartbeatTerminalSession ", err: errors.New("connection reset")})
		ended, reason := tbeatRun(t, a, tbeatPreviewSession())
		if ended != "" || reason != "" {
			t.Fatalf("ended=%q cut=%q — a transient database error killed the session", ended, reason)
		}
	})
	t.Run("activity write fails", func(t *testing.T) {
		a, db := tbeatAPI(t)
		db.rule(netcovRule{match: "-- name: RecordPreviewActivity ", err: errors.New("connection reset")})
		ended, reason := tbeatRun(t, a, tbeatPreviewSession())
		if ended != "" || reason != "" {
			t.Fatalf("ended=%q cut=%q — a lost activity beat is imprecision, not a close", ended, reason)
		}
	})
	// Zero rows updated is the one durable answer that DOES end the socket:
	// another replica, the sweep or a re-claim has finalized the row, and the
	// PTY must not outlive its own authorization — with the word that row
	// carries, which TestTbeatFinalizedElsewhereReportsThePersistedReason owns.
	t.Run("row already finalized", func(t *testing.T) {
		a, db := tbeatAPI(t)
		db.rule(netcovRule{match: "-- name: HeartbeatTerminalSession ", tag: "UPDATE 0"})
		db.rule(netcovRule{
			match: "-- name: GetTerminalSessionEndReason ",
			typed: []any{ptr(store.TerminalEndReasonRevoked)},
		})
		if ended, _ := tbeatRun(t, a, tbeatSession()); ended != terminal.EndRevoked {
			t.Fatalf("ended = %q, want %q — a finalized session keeps neither its socket nor its silence",
				ended, terminal.EndRevoked)
		}
	})
}

// The cross-replica repair, at the beat's own level. The replica that decides a
// shell is over writes the reason and never holds the socket; the replica that
// does learns of it only by its liveness update matching nothing. Until now
// every one of these reached the developer as `disconnect` — the word that sends
// somebody to inspect their own laptop for a container an administrator stopped.
//
// The fallback arms are the deliberate half of the design: a row that says
// nothing, and a row that cannot be read, both keep today's `disconnect`,
// because it is the honest word when the control plane does not know.
func TestTbeatFinalizedElsewhereReportsThePersistedReason(t *testing.T) {
	cases := map[string]struct {
		arrange func(db *tbeatDB)
		want    terminal.EndReason
	}{
		"container stopped under the shell (ADR-067 §2)": {
			arrange: func(db *tbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetTerminalSessionEndReason ",
					typed: []any{ptr(store.TerminalEndReasonTargetStopped)},
				})
			},
			want: terminalEndReasonTargetStopped,
		},
		"wake never came up (ADR-067)": {
			arrange: func(db *tbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetTerminalSessionEndReason ",
					typed: []any{ptr(store.TerminalEndReasonWakeFailed)},
				})
			},
			want: terminal.EndReason(store.TerminalEndReasonWakeFailed),
		},
		"revoked": {
			arrange: func(db *tbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetTerminalSessionEndReason ",
					typed: []any{ptr(store.TerminalEndReasonRevoked)},
				})
			},
			want: terminal.EndRevoked,
		},
		"the sweep called it a disconnect": {
			arrange: func(db *tbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetTerminalSessionEndReason ",
					typed: []any{ptr(store.TerminalEndReasonDisconnect)},
				})
			},
			want: terminal.EndDisconnect,
		},
		// The row is still open: this attach lost a re-claim (ADR-065 §5) rather
		// than the session ending, so there is no persisted word to report.
		"row still open — superseded attach": {
			arrange: func(db *tbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetTerminalSessionEndReason ",
					typed: []any{(*store.TerminalEndReason)(nil)},
				})
			},
			want: terminal.EndDisconnect,
		},
		"row purged before it could be read": {
			arrange: func(db *tbeatDB) {
				db.rule(netcovRule{match: "-- name: GetTerminalSessionEndReason ", noRows: true})
			},
			want: terminal.EndDisconnect,
		},
		"database unreachable": {
			arrange: func(db *tbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetTerminalSessionEndReason ",
					err:   errors.New("connection reset"),
				})
			},
			want: terminal.EndDisconnect,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a, db := tbeatAPI(t)
			db.rule(netcovRule{match: "-- name: HeartbeatTerminalSession ", tag: "UPDATE 0"})
			tc.arrange(db)
			ended, cut := tbeatRun(t, a, tbeatSession())
			if ended != tc.want {
				t.Fatalf("ended = %q, want %q", ended, tc.want)
			}
			if cut != "" {
				t.Fatalf("cut = %q — a session already over is not cut again", cut)
			}
			if db.ran("GetResourceByID") {
				t.Error("a finalized session resolved its target anyway: the beat must leave, not probe")
			}
		})
	}
}

// The cost bound the design turns on: the extra read is paid ONLY by the beat
// that discovers the row is gone. A healthy 20-second beat stays one statement
// against its own table, or the repair becomes a per-session query storm.
func TestTbeatHealthyBeatNeverReadsTheEndReason(t *testing.T) {
	a, db := tbeatAPI(t)
	tbeatInspect(t, a, `{"State":{"Running":true}}`, nil)
	if ended, cut := tbeatRun(t, a, tbeatSession()); ended != "" || cut != "" {
		t.Fatalf("ended=%q cut=%q — a healthy shell was ended", ended, cut)
	}
	if db.ran("GetTerminalSessionEndReason") {
		t.Fatal("the common path read the end reason: the beat must stay one statement while the row lives")
	}
}

// §2: a target that is definitely gone ends the session within one beat, and
// with a word that is true. The daemon does close a hijacked exec when its
// container dies, so the stream ends on its own — what it never says is WHY,
// and a shell that ends as `disconnect` sends a developer to inspect their own
// network for a container somebody stopped.
func TestTbeatHeartbeatEndsTheSessionWhenTheTargetIsGone(t *testing.T) {
	t.Run("container removed", func(t *testing.T) {
		a, _ := tbeatAPI(t)
		tbeatInspect(t, a, "", &agentwire.Error{Code: agentwire.CodeNotFound, Message: "no such container"})
		ended, reason := tbeatRun(t, a, tbeatSession())
		if reason != terminalEndReasonTargetStopped {
			t.Fatalf("cut = %q, want target_stopped — the value the client prints", reason)
		}
		// The beat itself stays silent: this session ends through the cut, which
		// carries the reason. The beat's own return is reserved for a row
		// somebody ELSE finalized.
		if ended != "" {
			t.Errorf("ended = %q — the reason must reach the bridge through the cut", ended)
		}
	})
	t.Run("container stopped but still present", func(t *testing.T) {
		a, _ := tbeatAPI(t)
		tbeatInspect(t, a, `{"State":{"Running":false}}`, nil)
		if _, reason := tbeatRun(t, a, tbeatSession()); reason != terminalEndReasonTargetStopped {
			t.Fatalf("reason = %q, want target_stopped: a stopped container has no shell either", reason)
		}
	})
	t.Run("destroyed preview", func(t *testing.T) {
		a, db := tbeatAPI(t)
		db.rule(netcovRule{match: "-- name: GetPreviewByID ", typed: []any{store.PreviewStatusDestroyed}})
		if _, reason := tbeatRun(t, a, tbeatPreviewSession()); reason != terminalEndReasonTargetStopped {
			t.Fatalf("reason = %q, want target_stopped for a preview that no longer exists", reason)
		}
	})
	t.Run("deleted resource row", func(t *testing.T) {
		a, db := tbeatAPI(t)
		db.rule(netcovRule{match: "-- name: GetResourceByID ", noRows: true})
		if _, reason := tbeatRun(t, a, tbeatSession()); reason != terminalEndReasonTargetStopped {
			t.Fatalf("reason = %q, want target_stopped: a resource that is gone has no container", reason)
		}
	})
	t.Run("target still running", func(t *testing.T) {
		a, _ := tbeatAPI(t)
		tbeatInspect(t, a, `{"State":{"Running":true}}`, nil)
		if _, reason := tbeatRun(t, a, tbeatSession()); reason != "" {
			t.Fatalf("reason = %q, want a healthy shell left alone", reason)
		}
	})
}

// The false-positive guard, which matters more than the fix: an agent channel
// is unavailable every time the helper restarts or the relay reconnects, and
// reading that silence as "the target is gone" would tear down every healthy
// shell on that server — trading a rare hang for a routine one.
func TestTbeatHeartbeatKeepsTheSessionWhenTheAgentIsMerelyUnavailable(t *testing.T) {
	cases := map[string]func(t *testing.T, a *API, db *tbeatDB){
		"no agent registry at all": func(*testing.T, *API, *tbeatDB) {},
		"agent not connected": func(_ *testing.T, a *API, _ *tbeatDB) {
			a.AgentRPC = &AgentConns{}
		},
		"channel answers unavailable": func(t *testing.T, a *API, _ *tbeatDB) {
			tbeatInspect(t, a, "", &agentwire.Error{Code: agentwire.CodeUnavailable, Message: "restarting"})
		},
		"inspect fails for some other reason": func(t *testing.T, a *API, _ *tbeatDB) {
			tbeatInspect(t, a, "", &agentwire.Error{Code: agentwire.CodeInternal, Message: "daemon busy"})
		},
		// A nil State is an answer we cannot read, which is not an absence.
		"state cannot be read": func(t *testing.T, a *API, _ *tbeatDB) {
			tbeatInspect(t, a, `{}`, nil)
		},
		// Same rule one layer up: a database that times out has said nothing
		// about the container, unlike a row that is definitively absent.
		"resource lookup fails transiently": func(_ *testing.T, a *API, db *tbeatDB) {
			db.rule(netcovRule{match: "-- name: GetResourceByID ", err: errors.New("connection reset")})
		},
		"preview lookup fails transiently": func(_ *testing.T, a *API, db *tbeatDB) {
			db.rule(netcovRule{match: "-- name: GetPreviewByID ", err: errors.New("connection reset")})
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			a, db := tbeatAPI(t)
			arrange(t, a, db)
			row := tbeatSession()
			if strings.Contains(name, "preview") {
				row = tbeatPreviewSession()
			}
			ended, reason := tbeatRun(t, a, row)
			if ended != "" || reason != "" {
				t.Fatalf("ended=%q cut=%q — an unreadable target is not an absent one", ended, reason)
			}
		})
	}
}

// A server shell has no container to probe: it runs as ssh_user over SSH, its
// liveness is the connection's own business, and it must survive a beat that
// would condemn any container-backed session.
func TestTbeatHeartbeatNeverProbesAServerShell(t *testing.T) {
	a, db := tbeatAPI(t)
	tbeatInspect(t, a, "", &agentwire.Error{Code: agentwire.CodeNotFound, Message: "no such container"})

	ended, reason := tbeatRun(t, a, tbeatServerShell())
	if ended != "" || reason != "" {
		t.Fatalf("ended=%q cut=%q — a server shell has no container to lose", ended, reason)
	}
	if db.ran("GetResourceByID") {
		t.Error("a server shell must not resolve a resource: it has none")
	}
}

// The beat must not become the thing that stalls a shell: its steps are
// bounded, and a caller whose context is already dead still gets a decision
// rather than a hang.
func TestTbeatHeartbeatIsBoundedAndOutlivesItsCaller(t *testing.T) {
	a, db := tbeatAPI(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if ended := a.terminalHeartbeat(tbeatSession())(ctx); ended != "" {
		t.Fatalf("a cancelled caller context closed the session as %q", ended)
	}
	if elapsed := time.Since(start); elapsed > 2*terminalBeatBudget {
		t.Fatalf("beat took %s, longer than its own budget", elapsed)
	}
	// The work still happened: the beat detaches from the caller's
	// cancellation on purpose, because the session it records is still open.
	if !db.ran("HeartbeatTerminalSession") {
		t.Error("the beat must run on a context of its own, not the caller's")
	}
}

// The test that would have caught the two bridges drifting apart (ADR-067 §1's
// last verification item). ADR-036's clause is one rule and this ADR gives it
// two implementations; what keeps them one rule is that the same target state
// produces the same set of writes on both.
func TestTbeatBothBridgesWriteTheSameSignals(t *testing.T) {
	terminalAPI, terminalDB := tbeatAPI(t)
	if ended, reason := tbeatRun(t, terminalAPI, tbeatSession()); ended != "" || reason != "" {
		t.Fatalf("terminal beat: ended=%q cut=%q", ended, reason)
	}

	tunnelAPI, tunnelDB := tbeatAPI(t)
	server, resource := int64(1), int64(9)
	tunnelRow := store.PortForwardSession{
		ID: 42, TeamID: 1, ServerID: &server, ResourceID: &resource,
		TargetName: "shop", TargetPort: 5432,
	}
	cancel := tunnelAPI.Tunnels.register(tunnelRow.ID)
	defer tunnelAPI.Tunnels.unregister(tunnelRow.ID, cancel)
	if ended := tunnelAPI.portForwardHeartbeat(tunnelRow)(context.Background()); ended != "" {
		t.Fatalf("port-forward beat ended a healthy session as %q", ended)
	}

	// Everything except the statement that names the family's own table.
	for _, name := range []string{"GetResourceByID", "RecordApplicationActivity", "RecordPreviewActivity"} {
		if got, want := terminalDB.ran(name), tunnelDB.ran(name); got != want {
			t.Errorf("%s: terminal ran it = %v, tunnel ran it = %v — the two beats have drifted", name, got, want)
		}
	}
	if !terminalDB.ran("HeartbeatTerminalSession") || !tunnelDB.ran("HeartbeatPortForwardSession") {
		t.Error("each family persists liveness on its own row")
	}
}

// §2's last clause: EVERY rung enforces it. ADR-064 put the WebSocket bridge
// and the HTTP session on the same bounds, so a shell must not behave
// differently for having landed on one rather than the other — and until this
// ADR the WebSocket rung registered nothing at all, which made a shell that
// landed there unreachable by any cut whatsoever.
//
// Both subtests prove the shell is genuinely pumping bytes before the target is
// taken away underneath it: a cut that landed on a session not yet bridging
// would prove nothing about what the developer reads.
func TestTbeatEveryRungReportsAVanishedTarget(t *testing.T) {
	claim := store.TerminalSession{
		ID: 7, TeamID: 1, AttachSeq: 1,
		TargetKind: store.TerminalTargetContainer, ServerID: ptr(int64(1)), ResourceID: ptr(int64(1)),
	}
	row := claim
	_ = row.Uuid.Scan(fixtureUUID)

	// The exec attach the container terminal rides (ADR-052 §5), echoing what
	// is typed into it — the observable that says the PTY and the socket are
	// joined.
	execScript := func(cmd agentwire.Command) (*agentwire.Result, bool) {
		switch cmd.Method {
		case agentwire.MethodContainerExecCreate:
			return &agentwire.Result{Body: json.RawMessage(`{"Id":"e1"}`)}, false
		case agentwire.MethodContainerExecAttach:
			return &agentwire.Result{}, true
		}
		return &agentwire.Result{Err: &agentwire.Error{Code: agentwire.CodeInvalid, Message: "unexpected"}}, false
	}

	t.Run("websocket", func(t *testing.T) {
		a, db := termclaimAPI(t)
		netcovTerminalClaim(db.netcovDB, claim)
		netcovAgent(t, a, execScript)

		srv := httptest.NewServer(http.HandlerFunc(a.TerminalWebSocket))
		defer srv.Close()
		conn := netcovDialWS(t, srv.URL+"?token=x")

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := conn.Write(ctx, websocket.MessageBinary, []byte("ping")); err != nil {
			t.Fatal(err)
		}
		if got := netcovReadBinary(t, conn); string(got) != "ping" {
			t.Fatalf("echo = %q — the shell is not bridging yet", got)
		}

		a.cutTerminalOnStoppedTarget(row, "the target container is no longer running")

		if got := tbeatReadEndFrame(t, conn); got != terminalEndReasonTargetStopped {
			t.Fatalf("end frame reason = %q, want %q", got, terminalEndReasonTargetStopped)
		}
		tbeatAssertPersistedReason(t, db)
	})

	t.Run("http", func(t *testing.T) {
		a, db := termclaimAPI(t)
		netcovTerminalClaim(db.netcovDB, claim)
		netcovAgent(t, a, execScript)

		key, _ := freshAttachKey(t)
		session := termclaimOpenSession(t, a, key, "?token=tk")
		session.awaitHead(t, 2*time.Second)
		stream := termclaimOpenStream(t, a, key, session.writer.Header().Get(tunnel.TerminalHTTP.SessionHeader))
		stream.awaitHead(t, 2*time.Second)

		if _, err := stream.body.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		tbeatEventually(t, func() bool { return strings.Contains(stream.writer.wire(), "ping") },
			"the shell never echoed — it is not bridging yet")

		a.cutTerminalOnStoppedTarget(row, "the target container is no longer running")
		session.awaitDone(t)

		if wire := session.writer.wire(); !strings.Contains(wire, `"reason":"target_stopped"`) {
			t.Fatalf("session wire = %s — the vanished target must be named on the control wire", wire)
		}
		tbeatAssertPersistedReason(t, db)
	})
}

// tbeatReadEndFrame reads the session's final text frame off a WebSocket.
func tbeatReadEndFrame(t *testing.T, conn *websocket.Conn) terminal.EndReason {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			t.Fatalf("no end frame before the socket closed: %v", err)
		}
		if typ != websocket.MessageText {
			continue
		}
		var msg struct {
			Type   string             `json:"type"`
			Reason terminal.EndReason `json:"reason"`
		}
		if json.Unmarshal(data, &msg) == nil && msg.Type == "end" {
			return msg.Reason
		}
	}
}

// tbeatAssertPersistedReason checks the row and the wire came from one value —
// `revoked` being the specific lie ADR-067 §2 is about, since a cut used to
// reach the bridge as nothing but a cancelled context.
func tbeatAssertPersistedReason(t *testing.T, db *termclaimDB) {
	t.Helper()
	var end termclaimCall
	tbeatEventually(t, func() bool {
		var ok bool
		end, ok = db.calledWith("-- name: EndTerminalSession ")
		return ok
	}, "the cut session was never finalized")
	switch got := termclaimEndReason(t, end); got {
	case store.TerminalEndReasonTargetStopped:
	case store.TerminalEndReasonRevoked:
		t.Fatal("the row records a revocation nobody performed")
	default:
		t.Fatalf("end reason = %q, want %q", got, store.TerminalEndReasonTargetStopped)
	}
}
