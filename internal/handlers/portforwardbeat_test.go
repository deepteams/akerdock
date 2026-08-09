// The port-forward heartbeat: the 20-second beat both transports run (ADR-032
// over ADR-064). It is the only moment a tunnel talks to the control plane
// while a developer sits idle in psql, which is why two otherwise unrelated
// duties ride on it — telling scale-to-zero somebody is connected, and noticing
// that the target container is gone.
//
// Every top-level identifier is prefixed pfbeat (concurrent-agent rule).
package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/api"
	"github.com/deepteams/akerdock/internal/store"
	"github.com/deepteams/akerdock/internal/tunnel"
)

// pfbeatDB wraps the steerable netcov fake with a call log. What a beat does is
// CHOOSE a statement — the preview one, the application one, or none at all —
// and those statements return nothing a response could show, so the log is the
// only place that choice is observable.
type pfbeatDB struct {
	*netcovDB
	mu    sync.Mutex
	calls []string
}

func (db *pfbeatDB) record(sql string) {
	db.mu.Lock()
	defer db.mu.Unlock()
	db.calls = append(db.calls, sql)
}

func (db *pfbeatDB) ran(name string) bool {
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, sql := range db.calls {
		if strings.Contains(sql, "-- name: "+name+" ") {
			return true
		}
	}
	return false
}

func (db *pfbeatDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	db.record(sql)
	return db.netcovDB.Exec(ctx, sql, args...)
}

func (db *pfbeatDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	db.record(sql)
	return db.netcovDB.Query(ctx, sql, args...)
}

func (db *pfbeatDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	db.record(sql)
	return db.netcovDB.QueryRow(ctx, sql, args...)
}

var _ store.DBTX = (*pfbeatDB)(nil)

func pfbeatAPI(t *testing.T) (*API, *pfbeatDB) {
	t.Helper()
	a, inner := netcovAPI(t)
	db := &pfbeatDB{netcovDB: inner}
	a.Store = store.New(db)
	return a, db
}

// pfbeatSession is a claimed session against server 1 — the server netcovAgent
// registers its scripted channel for.
func pfbeatSession() store.PortForwardSession {
	server, resource := int64(1), int64(9)
	return store.PortForwardSession{
		ID: 42, ServerID: &server, ResourceID: &resource, TeamID: 1,
		TargetName: "shop", TargetPort: 5432,
	}
}

func pfbeatPreviewSession() store.PortForwardSession {
	row := pfbeatSession()
	preview := int64(11)
	row.PreviewID = &preview
	return row
}

// pfbeatInspect scripts the agent channel's ContainerInspect answer.
func pfbeatInspect(t *testing.T, a *API, body string, failure *agentwire.Error) {
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

// pfbeatRun beats once and reports both of the beat's outputs: the reason it
// ENDED the session with, empty while the session lives, and the reason it CUT
// the session with through the registry a revocation uses. They are different
// channels on purpose — a target that vanished is cut, so the reason travels the
// same path an operator's close does, while a row finalized on another replica
// is already over and the beat carries its word straight back to the bridge.
func pfbeatRun(t *testing.T, a *API, row store.PortForwardSession) (ended, cut tunnel.EndReason) {
	t.Helper()
	cancel := a.Tunnels.register(row.ID)
	defer a.Tunnels.unregister(row.ID, cancel)
	ended = a.portForwardHeartbeat(row)(context.Background())
	select {
	case reason := <-cancel:
		return ended, reason
	default:
		return ended, ""
	}
}

// A tunnel is activity for the resource it is attached to — the signal the
// waker cannot produce, since a port-forward reaches the container's IP over
// SSH and never crosses the proxy that writes the activity file.
func TestPfbeatHeartbeatRecordsActivityForItsTarget(t *testing.T) {
	cases := map[string]struct {
		row     store.PortForwardSession
		arrange func(db *pfbeatDB)
		preview bool
		app     bool
	}{
		"preview": {row: pfbeatPreviewSession(), preview: true},
		"application": {
			row: pfbeatSession(), app: true,
		},
		// A database has no scale-to-zero at all: there is no clock to reset,
		// and writing one would be inventing the semantics with it.
		"database": {
			row: pfbeatSession(),
			arrange: func(db *pfbeatDB) {
				db.rule(netcovRule{match: "-- name: GetResourceByID ", typed: []any{store.ResourceTypeDatabase}})
			},
		},
		// ADR-045: an external endpoint is an address frozen at declaration.
		// Nothing behind it is ours to keep awake — or even to look at.
		"external endpoint": {
			row: func() store.PortForwardSession {
				row := pfbeatSession()
				endpoint := int64(3)
				row.ExternalEndpointID = &endpoint
				row.ResourceID = nil
				return row
			}(),
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a, db := pfbeatAPI(t)
			if tc.arrange != nil {
				tc.arrange(db)
			}
			ended, reason := pfbeatRun(t, a, tc.row)
			if ended != "" || reason != "" {
				t.Fatalf("beat ended the session (ended=%q cut=%q) — nothing here is a reason to", ended, reason)
			}
			if got := db.ran("RecordPreviewActivity"); got != tc.preview {
				t.Errorf("preview activity recorded = %v, want %v", got, tc.preview)
			}
			if got := db.ran("RecordApplicationActivity"); got != tc.app {
				t.Errorf("application activity recorded = %v, want %v", got, tc.app)
			}
			if !db.ran("HeartbeatPortForwardSession") {
				t.Error("liveness must still be persisted: it is the crash/restart net")
			}
		})
	}
}

// The window the first beat cannot cover (ADR-067 §1): a tunnel is opened
// precisely because nothing has touched the resource lately, so the mint races
// the sleep decision at 29:50 of a 30-minute window — and a stamp that waits
// twenty seconds for the first heartbeat arrives after the containers stopped.
func TestPfbeatMintStampsActivityForItsTarget(t *testing.T) {
	body := api.PortForwardCreate{Port: 5432}
	cases := map[string]struct {
		arrange func(db *pfbeatDB)
		mint    func(a *API, rec *httptest.ResponseRecorder, r *http.Request)
		body    any
		preview bool
		app     bool
	}{
		"application": {
			body: body,
			mint: func(a *API, rec *httptest.ResponseRecorder, r *http.Request) {
				a.CreateApplicationPortForward(rec, r, fixtureUUID, api.CreateApplicationPortForwardParams{})
			},
			app: true,
		},
		"preview": {
			body: body,
			mint: func(a *API, rec *httptest.ResponseRecorder, r *http.Request) {
				a.CreatePreviewPortForward(rec, r, fixtureUUID, fixtureUUID, api.CreatePreviewPortForwardParams{})
			},
			preview: true,
		},
		// ADR-037 §2 excludes databases from scale-to-zero by construction.
		"database": {
			body: body,
			mint: func(a *API, rec *httptest.ResponseRecorder, r *http.Request) {
				a.CreateDatabasePortForward(rec, r, fixtureUUID)
			},
		},
		// ADR-045: a frozen address on somebody else's infrastructure — there
		// is nothing of ours to keep awake.
		"external endpoint": {
			arrange: func(db *pfbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetExternalEndpointByUUID ",
					typed: []any{store.ExternalEndpointCriticalityStandard},
				})
			},
			mint: func(a *API, rec *httptest.ResponseRecorder, r *http.Request) {
				a.CreateExternalEndpointPortForward(rec, r, fixtureUUID)
			},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a, db := pfbeatAPI(t)
			if tc.arrange != nil {
				tc.arrange(db)
			}
			rec := httptest.NewRecorder()
			tc.mint(a, rec, netcovRequest(t, http.MethodPost, "/pf", tc.body, netcovIdentity()))
			netcovStatus(t, rec, http.StatusCreated)
			if got := db.ran("RecordPreviewActivity"); got != tc.preview {
				t.Errorf("preview activity stamped at mint = %v, want %v", got, tc.preview)
			}
			if got := db.ran("RecordApplicationActivity"); got != tc.app {
				t.Errorf("application activity stamped at mint = %v, want %v", got, tc.app)
			}
		})
	}
	// A timestamp is never a reason to refuse a tunnel.
	t.Run("a failed stamp does not fail the mint", func(t *testing.T) {
		a, db := pfbeatAPI(t)
		db.rule(netcovRule{match: "-- name: RecordApplicationActivity ", err: errors.New("connection reset")})
		rec := httptest.NewRecorder()
		a.CreateApplicationPortForward(rec, netcovRequest(t, http.MethodPost, "/pf", body, netcovIdentity()),
			fixtureUUID, api.CreateApplicationPortForwardParams{})
		netcovStatus(t, rec, http.StatusCreated)
	})
}

// The beat's contract on failure: the socket is the source of truth while this
// process is alive, so a database that blinks is logged and retried 20 seconds
// later — never a reason to drop a working tunnel.
func TestPfbeatHeartbeatSurvivesADatabaseFailure(t *testing.T) {
	t.Run("liveness update fails", func(t *testing.T) {
		a, db := pfbeatAPI(t)
		db.rule(netcovRule{match: "-- name: HeartbeatPortForwardSession ", err: errors.New("connection reset")})
		ended, reason := pfbeatRun(t, a, pfbeatPreviewSession())
		if ended != "" || reason != "" {
			t.Fatalf("ended=%q cut=%q — a transient database error killed the session", ended, reason)
		}
	})
	t.Run("activity write fails", func(t *testing.T) {
		a, db := pfbeatAPI(t)
		db.rule(netcovRule{match: "-- name: RecordPreviewActivity ", err: errors.New("connection reset")})
		ended, reason := pfbeatRun(t, a, pfbeatPreviewSession())
		if ended != "" || reason != "" {
			t.Fatalf("ended=%q cut=%q — a lost activity beat is imprecision, not a close", ended, reason)
		}
	})
	// Zero rows updated is the one durable answer that DOES end the socket:
	// another replica or the scheduler has finalized the row, and the session
	// must not outlive its own authorization. It must also end it with the word
	// that row carries — see TestPfbeatFinalizedElsewhereReportsThePersistedReason.
	t.Run("row already finalized", func(t *testing.T) {
		a, db := pfbeatAPI(t)
		db.rule(netcovRule{match: "-- name: HeartbeatPortForwardSession ", tag: "UPDATE 0"})
		db.rule(netcovRule{
			match: "-- name: GetPortForwardSessionEndReason ",
			typed: []any{ptr(store.TerminalEndReasonGrantExpired)},
		})
		if ended, _ := pfbeatRun(t, a, pfbeatSession()); ended != endReasonGrantExpired {
			t.Fatalf("ended = %q, want %q — a finalized session keeps neither its socket nor its silence",
				ended, endReasonGrantExpired)
		}
	})
}

// The cross-replica repair, at the beat's own level: the replica that decides a
// session is over writes the reason and never touches the socket, so the replica
// that HOLDS the socket only ever sees its liveness update match nothing. Before
// this, every one of these arrived at the developer as `disconnect` — a network
// glitch, for a grant that expired or a container somebody stopped.
//
// The fallback arms are the deliberate half: a row that says nothing and a row
// that cannot be read both keep today's `disconnect`, because that is the honest
// word when the control plane does not know.
func TestPfbeatFinalizedElsewhereReportsThePersistedReason(t *testing.T) {
	cases := map[string]struct {
		arrange func(db *pfbeatDB)
		want    tunnel.EndReason
	}{
		"target stopped under the tunnel": {
			arrange: func(db *pfbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetPortForwardSessionEndReason ",
					typed: []any{ptr(store.TerminalEndReasonTargetStopped)},
				})
			},
			want: endReasonTargetStopped,
		},
		"grant ran out (ADR-045 §5)": {
			arrange: func(db *pfbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetPortForwardSessionEndReason ",
					typed: []any{ptr(store.TerminalEndReasonGrantExpired)},
				})
			},
			want: endReasonGrantExpired,
		},
		"wake never came up (ADR-067)": {
			arrange: func(db *pfbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetPortForwardSessionEndReason ",
					typed: []any{ptr(store.TerminalEndReasonWakeFailed)},
				})
			},
			want: endReasonWakeFailed,
		},
		"grant revoked": {
			arrange: func(db *pfbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetPortForwardSessionEndReason ",
					typed: []any{ptr(store.TerminalEndReasonRevoked)},
				})
			},
			want: tunnel.EndReason(store.TerminalEndReasonRevoked),
		},
		// The row is still open: this attach lost a re-claim (ADR-065 §5) rather
		// than the session ending, so there is no persisted word to report.
		"row still open — superseded attach": {
			arrange: func(db *pfbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetPortForwardSessionEndReason ",
					typed: []any{(*store.TerminalEndReason)(nil)},
				})
			},
			want: tunnel.EndDisconnect,
		},
		"row purged before it could be read": {
			arrange: func(db *pfbeatDB) {
				db.rule(netcovRule{match: "-- name: GetPortForwardSessionEndReason ", noRows: true})
			},
			want: tunnel.EndDisconnect,
		},
		"database unreachable": {
			arrange: func(db *pfbeatDB) {
				db.rule(netcovRule{
					match: "-- name: GetPortForwardSessionEndReason ",
					err:   errors.New("connection reset"),
				})
			},
			want: tunnel.EndDisconnect,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			a, db := pfbeatAPI(t)
			db.rule(netcovRule{match: "-- name: HeartbeatPortForwardSession ", tag: "UPDATE 0"})
			tc.arrange(db)
			ended, cut := pfbeatRun(t, a, pfbeatSession())
			if ended != tc.want {
				t.Fatalf("ended = %q, want %q", ended, tc.want)
			}
			if cut != "" {
				t.Fatalf("cut = %q — a session already over is not cut again", cut)
			}
			// A row that is gone is not probed: the target's state cannot change
			// what already happened, and the beat leaves rather than spending its
			// budget on the agent channel.
			if db.ran("GetResourceByID") {
				t.Error("a finalized session resolved its target anyway")
			}
		})
	}
}

// The cost bound the design turns on: the extra read is paid ONLY by the beat
// that discovers the row is gone. A healthy 20-second beat must stay one
// statement against its own table, or the repair becomes a per-session query
// storm on the largest deployments.
func TestPfbeatHealthyBeatNeverReadsTheEndReason(t *testing.T) {
	a, db := pfbeatAPI(t)
	pfbeatInspect(t, a, `{"State":{"Running":true}}`, nil)
	if ended, cut := pfbeatRun(t, a, pfbeatSession()); ended != "" || cut != "" {
		t.Fatalf("ended=%q cut=%q — a healthy tunnel was ended", ended, cut)
	}
	if db.ran("GetPortForwardSessionEndReason") {
		t.Fatal("the common path read the end reason: the beat must stay one statement while the row lives")
	}
}

// The hang this exists to fix: when the target container is gone, the forwarded
// connection gets no RST and no FIN — it black-holes. One beat has to turn that
// silence into a close the CLI can print.
func TestPfbeatHeartbeatEndsTheSessionWhenTheTargetIsGone(t *testing.T) {
	t.Run("container removed", func(t *testing.T) {
		a, _ := pfbeatAPI(t)
		pfbeatInspect(t, a, "", &agentwire.Error{Code: agentwire.CodeNotFound, Message: "no such container"})
		ended, reason := pfbeatRun(t, a, pfbeatSession())
		if reason != endReasonTargetStopped {
			t.Fatalf("cut = %q, want target_stopped — the value the CLI turns into an error", reason)
		}
		// The beat itself stays silent: the session ends through the cut, which
		// carries the reason, not through the beat's own return — that one is
		// reserved for a row somebody ELSE finalized.
		if ended != "" {
			t.Errorf("ended = %q — the reason must reach the bridge through the cut", ended)
		}
	})
	t.Run("container stopped but still present", func(t *testing.T) {
		a, _ := pfbeatAPI(t)
		pfbeatInspect(t, a, `{"State":{"Running":false}}`, nil)
		if _, reason := pfbeatRun(t, a, pfbeatSession()); reason != endReasonTargetStopped {
			t.Fatalf("reason = %q, want target_stopped: a stopped container black-holes exactly like a removed one", reason)
		}
	})
	t.Run("destroyed preview", func(t *testing.T) {
		a, db := pfbeatAPI(t)
		db.rule(netcovRule{match: "-- name: GetPreviewByID ", typed: []any{store.PreviewStatusDestroyed}})
		if _, reason := pfbeatRun(t, a, pfbeatPreviewSession()); reason != endReasonTargetStopped {
			t.Fatalf("reason = %q, want target_stopped for a preview that no longer exists", reason)
		}
	})
	t.Run("deleted resource row", func(t *testing.T) {
		a, db := pfbeatAPI(t)
		db.rule(netcovRule{match: "-- name: GetResourceByID ", noRows: true})
		if _, reason := pfbeatRun(t, a, pfbeatSession()); reason != endReasonTargetStopped {
			t.Fatalf("reason = %q, want target_stopped: a resource that is gone has no container", reason)
		}
	})
	t.Run("target still running", func(t *testing.T) {
		a, _ := pfbeatAPI(t)
		pfbeatInspect(t, a, `{"State":{"Running":true}}`, nil)
		if _, reason := pfbeatRun(t, a, pfbeatSession()); reason != "" {
			t.Fatalf("reason = %q, want a healthy tunnel left alone", reason)
		}
	})
}

// The false-positive guard, which matters more than the fix: an agent channel
// is unavailable every time the helper restarts or the relay reconnects, and
// reading that silence as "the target is gone" would tear down healthy tunnels
// routinely — trading a rare hang for a frequent one.
func TestPfbeatHeartbeatKeepsTheSessionWhenTheAgentIsMerelyUnavailable(t *testing.T) {
	cases := map[string]func(t *testing.T, a *API, db *pfbeatDB){
		"no agent registry at all": func(*testing.T, *API, *pfbeatDB) {},
		"agent not connected": func(_ *testing.T, a *API, _ *pfbeatDB) {
			a.AgentRPC = &AgentConns{}
		},
		"channel answers unavailable": func(t *testing.T, a *API, _ *pfbeatDB) {
			pfbeatInspect(t, a, "", &agentwire.Error{Code: agentwire.CodeUnavailable, Message: "restarting"})
		},
		"inspect fails for some other reason": func(t *testing.T, a *API, _ *pfbeatDB) {
			pfbeatInspect(t, a, "", &agentwire.Error{Code: agentwire.CodeInternal, Message: "daemon busy"})
		},
		// Same rule one layer up: a database that times out has said nothing
		// about the container, unlike a row that is definitively absent.
		"resource lookup fails transiently": func(_ *testing.T, a *API, db *pfbeatDB) {
			pfbeatInspect(t, a, `{"State":{"Running":true}}`, nil)
			db.rule(netcovRule{match: "-- name: GetResourceByID ", err: errors.New("connection reset")})
		},
		"preview lookup fails transiently": func(_ *testing.T, a *API, db *pfbeatDB) {
			pfbeatInspect(t, a, `{"State":{"Running":true}}`, nil)
			db.rule(netcovRule{match: "-- name: GetPreviewByID ", err: errors.New("connection reset")})
		},
	}
	for name, arrange := range cases {
		t.Run(name, func(t *testing.T) {
			a, db := pfbeatAPI(t)
			arrange(t, a, db)
			row := pfbeatSession()
			if strings.Contains(name, "preview") {
				row = pfbeatPreviewSession()
			}
			ended, reason := pfbeatRun(t, a, row)
			if ended != "" || reason != "" {
				t.Fatalf("ended=%q cut=%q — an unreadable target is not an absent one", ended, reason)
			}
		})
	}
}

// An external-endpoint session has no container to probe: the address was
// frozen at declaration (ADR-045) and the far side belongs to somebody else.
// It must survive a beat that would condemn any resource-backed session.
func TestPfbeatHeartbeatNeverProbesAnExternalEndpoint(t *testing.T) {
	a, db := pfbeatAPI(t)
	pfbeatInspect(t, a, "", &agentwire.Error{Code: agentwire.CodeNotFound, Message: "no such container"})
	row := pfbeatSession()
	endpoint := int64(3)
	row.ExternalEndpointID = &endpoint
	row.ResourceID = nil

	ended, reason := pfbeatRun(t, a, row)
	if ended != "" || reason != "" {
		t.Fatalf("ended=%q cut=%q — an endpoint session has no container to lose", ended, reason)
	}
	if db.ran("GetResourceByID") {
		t.Error("an endpoint session must not resolve a resource: it has none")
	}
}

// The beat must not become the thing that stalls a tunnel: its steps are
// bounded, and a caller whose context is already dead still gets a decision
// rather than a hang.
func TestPfbeatHeartbeatIsBoundedAndOutlivesItsCaller(t *testing.T) {
	a, db := pfbeatAPI(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if ended := a.portForwardHeartbeat(pfbeatSession())(ctx); ended != "" {
		t.Fatalf("a cancelled caller context closed the session as %q", ended)
	}
	if elapsed := time.Since(start); elapsed > 2*portForwardBeatBudget {
		t.Fatalf("beat took %s, longer than its own budget", elapsed)
	}
	// The work still happened: the beat detaches from the caller's
	// cancellation on purpose, because the session it records is still open.
	if !db.ran("HeartbeatPortForwardSession") {
		t.Error("the beat must run on a context of its own, not the caller's")
	}
}
