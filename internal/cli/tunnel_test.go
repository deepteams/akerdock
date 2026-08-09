package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// The migration case: the REF spelling `port-forward` used to take must answer
// with the bare form rather than fail to resolve (ADR-069 §2).
func TestEndpointName(t *testing.T) {
	cases := []struct {
		arg, want, wantErr string
	}{
		{arg: "prod-replica", want: "prod-replica"},
		{arg: "endpoint/prod-replica", wantErr: "akerdock tunnel open prod-replica"},
		{arg: "ep/prod-replica", wantErr: "akerdock tunnel open prod-replica"},
		{arg: "db/pg", wantErr: "invalid endpoint name"},
		{arg: "endpoint/", wantErr: "invalid endpoint name"},
	}
	for _, tc := range cases {
		got, err := endpointName(tc.arg)
		switch {
		case tc.wantErr == "" && (err != nil || got != tc.want):
			t.Errorf("endpointName(%q) = (%q, %v), want %q", tc.arg, got, err, tc.want)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("endpointName(%q) err = %v, want it to mention %q", tc.arg, err, tc.wantErr)
		}
	}
}

// A session's life is three fields the operator reads as one word — and when it
// is over, the word is the reason it ended, not "closed".
func TestTunnelStateAndTarget(t *testing.T) {
	now := time.Now()
	cases := []struct {
		session tunnelSession
		state   string
		target  string
		port    string
	}{
		{
			session: tunnelSession{Active: true, StartedAt: &now, TargetKind: "database", TargetName: "pg", TargetPort: 5432},
			state:   "attached", target: "database/pg", port: "5432",
		},
		{
			session: tunnelSession{Active: true, TargetKind: "application", TargetName: "varuna", TargetComponent: "postgres", TargetPort: 5432},
			state:   "pending", target: "application/varuna:postgres", port: "5432",
		},
		{
			// An external endpoint named no port at the mint (ADR-045 §2).
			session: tunnelSession{EndReason: "grant_expired", TargetKind: "external_endpoint", TargetName: "replica"},
			state:   "grant_expired", target: "external_endpoint/replica", port: "-",
		},
		{
			session: tunnelSession{EndedAt: &now, TargetKind: "preview", TargetName: "pr-8", TargetPort: 3000},
			state:   "closed", target: "preview/pr-8", port: "3000",
		},
	}
	for _, tc := range cases {
		if got := tunnelState(tc.session); got != tc.state {
			t.Errorf("tunnelState = %q, want %q", got, tc.state)
		}
		if got := tunnelTarget(tc.session); got != tc.target {
			t.Errorf("tunnelTarget = %q, want %q", got, tc.target)
		}
		if got := tunnelPort(tc.session); got != tc.port {
			t.Errorf("tunnelPort = %q, want %q", got, tc.port)
		}
	}
}

// `tunnel open <name> <port>` mints with an EMPTY body (asserted server-side by
// tunnelServer) and attaches; the refused upgrade ends the run with the
// server's own sentence.
func TestTunnelOpenMintsWithoutABody(t *testing.T) {
	srv := tunnelServer(t)
	setupContext(t, srv.URL)
	var err error
	_, _ = captureOutput(t, func() {
		err = runCmd(tunnelCmd(), "open", "replica", "15432")
	})
	if err == nil || !strings.Contains(err.Error(), "not reachable over SSH") {
		t.Fatalf("err = %v", err)
	}
}

// Without a local port the OS picks one, and the announcement names the
// endpoint's declared target instead of a remote port the caller never chose.
func TestTunnelOpenPicksTheLocalPort(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/external-endpoints", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"ep-1","name":"replica"}]}`))
	})
	mux.HandleFunc("/api/v1/external-endpoints/ep-1/port-forwards", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"websocket_path":"/quick","token":"tk"}`))
	})
	mux.HandleFunc("/quick", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"akerdock-tunnel-v1"}})
		if err != nil {
			return
		}
		// A deliberate user_close right away: the client exits silently.
		_ = conn.Close(websocket.StatusNormalClosure, "user_close")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	setupContext(t, srv.URL)

	var err error
	_, errOut := captureOutput(t, func() {
		err = runCmd(tunnelCmd(), "open", "replica")
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut, "the endpoint's declared target") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// The grant ceremony is ADR-045's, unchanged by the move: the mint is replayed
// until it succeeds, and the developer is sent to the page that issues one.
func TestTunnelOpenWaitsForAnAccessGrant(t *testing.T) {
	t.Setenv("PATH", "") // neutralize openBrowser: no launcher to run
	mints := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/external-endpoints", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"ep-1","name":"replica"}]}`))
	})
	mux.HandleFunc("/api/v1/external-endpoints/ep-1/port-forwards", func(w http.ResponseWriter, _ *http.Request) {
		mints++
		if mints == 1 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"code":"access_request_required","message":"needs a grant","request_url":"https://x/grant"}`))
			return
		}
		_, _ = w.Write([]byte(`{"websocket_path":"/refused","token":"tk"}`))
	})
	mux.HandleFunc("/refused", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"message":"the server is not reachable over SSH right now"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	setupContext(t, srv.URL)

	var err error
	_, errOut := captureOutput(t, func() {
		err = runCmd(tunnelCmd(), "open", "replica", "15432")
	})
	if mints < 2 {
		t.Fatalf("mints = %d — the CLI replays the mint until the grant exists", mints)
	}
	if !strings.Contains(errOut, "needs an access grant") {
		t.Fatalf("stderr = %q — the developer is told what is being waited on", errOut)
	}
	if err == nil || !strings.Contains(err.Error(), "not reachable over SSH") {
		t.Fatalf("err = %v", err)
	}
}

// tunnelSessionsServer serves one page of the team's sessions and records the
// query the CLI asked with.
func tunnelSessionsServer(t *testing.T, gotQuery *string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/external-endpoints", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"ep-1","name":"replica"}]}`))
	})
	mux.HandleFunc("/api/v1/port-forward-sessions", func(w http.ResponseWriter, r *http.Request) {
		*gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"data":[
			{"uuid":"s-1","target_kind":"external_endpoint","target_name":"replica","target_port":0,
			 "user_email":"jean-luc@example.com","active":true,"created_at":"2026-08-09T10:00:00Z","started_at":"2026-08-09T10:00:02Z"},
			{"uuid":"s-2","target_kind":"database","target_name":"pg","target_port":5432,
			 "active":false,"created_at":"2026-08-09T09:00:00Z","end_reason":"idle_timeout"}
		]}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// The listing spans every target kind — ADR-045 decided that question for the
// dashboard and it did not change for the CLI.
func TestTunnelLsShowsEveryTargetKind(t *testing.T) {
	var query string
	srv := tunnelSessionsServer(t, &query)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(tunnelCmd(), "ls") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"external_endpoint/replica", "database/pg", "attached", "idle_timeout", "s-1"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want it to contain %q", out, want)
		}
	}
	if strings.Contains(query, "active") {
		t.Fatalf("query = %q — the default listing is the live sessions, which is the API default", query)
	}
}

func TestTunnelLsFilters(t *testing.T) {
	t.Run("--endpoint resolves the name to its uuid", func(t *testing.T) {
		var query string
		srv := tunnelSessionsServer(t, &query)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() { err = runCmd(tunnelCmd(), "ls", "--endpoint", "replica") })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(query, "external_endpoint_uuid=ep-1") {
			t.Fatalf("query = %q", query)
		}
	})

	t.Run("--all walks the history", func(t *testing.T) {
		var query string
		srv := tunnelSessionsServer(t, &query)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() { err = runCmd(tunnelCmd(), "ls", "--all") })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(query, "active=false") {
			t.Fatalf("query = %q", query)
		}
	})

	t.Run("-o json emits the API objects", func(t *testing.T) {
		var query string
		srv := tunnelSessionsServer(t, &query)
		setupContext(t, srv.URL)
		flags.output = "json"
		t.Cleanup(func() { flags.output = "table" })
		var err error
		out, _ := captureOutput(t, func() { err = runCmd(tunnelCmd(), "ls") })
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		var sessions []tunnelSession
		if err := json.Unmarshal([]byte(out), &sessions); err != nil {
			t.Fatalf("stdout is not JSON: %v (%q)", err, out)
		}
		if len(sessions) != 2 || sessions[0].Uuid != "s-1" {
			t.Fatalf("sessions = %+v", sessions)
		}
	})
}

func TestTunnelClose(t *testing.T) {
	var method, path string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/port-forward-sessions/", func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(tunnelCmd(), "close", "s-1") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if method != http.MethodDelete || path != "/api/v1/port-forward-sessions/s-1" {
		t.Fatalf("%s %s", method, path)
	}
	if !strings.Contains(out, "session closed") {
		t.Fatalf("stdout = %q", out)
	}
}

// A refusal is the server's, and it is what the operator reads.
func TestTunnelCloseRefused(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/port-forward-sessions/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"closing another member's session needs external-endpoints:manage"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	setupContext(t, srv.URL)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(tunnelCmd(), "close", "s-1") })
	if err == nil || !strings.Contains(err.Error(), "external-endpoints:manage") {
		t.Fatalf("err = %v", err)
	}
}
