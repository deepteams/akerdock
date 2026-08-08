package cli

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStr(t *testing.T) {
	m := map[string]any{"a": "x", "b": 3}
	if str(m, "a") != "x" || str(m, "b") != "" || str(m, "missing") != "" {
		t.Fatal("str must return only string values")
	}
}

func TestEngineRecipe(t *testing.T) {
	for engine, wantBin := range map[string]string{
		"postgres": "psql", "postgresql": "psql",
		"mysql": "mysql", "mariadb": "mysql",
		"redis": "redis-cli", "keydb": "redis-cli", "dragonfly": "redis-cli",
		"mongo": "mongosh", "mongodb": "mongosh",
	} {
		rec, ok := engineRecipe(engine)
		if !ok || rec.bin != wantBin {
			t.Errorf("engineRecipe(%q) = %+v ok=%v, want bin %q", engine, rec, ok, wantBin)
		}
	}
	if _, ok := engineRecipe("sqlite"); ok {
		t.Error("sqlite has no client recipe")
	}
}

// Each recipe builds a working argv and credential environment from the
// database detail; these closures are what actually launch the console.
func TestEngineRecipesBuildClientCommands(t *testing.T) {
	detail := map[string]any{"username": "u", "password": "p", "database": "d"}

	pg := engines["postgres"]
	if got := pg.args("127.0.0.1", 15432, detail); !contains(got, "-U") || !contains(got, "u") || !contains(got, "d") {
		t.Errorf("psql args = %v", got)
	}
	// psql defaults the dbname to the user when the detail names none.
	if got := pg.args("127.0.0.1", 15432, map[string]any{"username": "u"}); got[len(got)-1] != "u" {
		t.Errorf("psql args without database = %v", got)
	}
	if got := pg.env(detail); !contains(got, "PGPASSWORD=p") {
		t.Errorf("psql env = %v", got)
	}

	my := engines["mysql"]
	if got := my.args("h", 3307, detail); !contains(got, "-u") || !contains(got, "3307") {
		t.Errorf("mysql args = %v", got)
	}
	if got := my.env(detail); !contains(got, "MYSQL_PWD=p") {
		t.Errorf("mysql env = %v", got)
	}

	rd := engines["redis"]
	if got := rd.args("h", 6380, detail); !contains(got, "6380") {
		t.Errorf("redis args = %v", got)
	}
	if got := rd.env(detail); !contains(got, "REDISCLI_AUTH=p") {
		t.Errorf("redis env = %v", got)
	}

	mg := engines["mongo"]
	if got := mg.args("h", 27018, detail); !contains(got, "mongodb://h:27018") {
		t.Errorf("mongo args = %v", got)
	}
	if got := mg.env(detail); got != nil {
		t.Errorf("mongo env = %v", got)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func TestNormalizeComponentID(t *testing.T) {
	for in, want := range map[string]string{
		"postgres":    "POSTGRES",
		"my-db_2":     "MY_DB_2",
		"Web.Service": "WEB_SERVICE",
	} {
		if got := normalizeComponentID(in); got != want {
			t.Errorf("normalizeComponentID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "x", "y"); got != "x" {
		t.Fatalf("got %q", got)
	}
	if got := firstNonEmpty(); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestFreePortAndWaitPort(t *testing.T) {
	port, err := freePort()
	if err != nil || port == 0 {
		t.Fatalf("port=%d err=%v", port, err)
	}
	// With a listener up, waitPort returns as soon as it connects.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	waitPort(port)
	// Without one it gives up after its bounded retries instead of hanging.
	idle, err := freePort()
	if err != nil {
		t.Fatal(err)
	}
	waitPort(idle)
}

// dbServer fakes the endpoints `akerdock db` walks for both a standalone
// database and a compose service, with credentials visible or redacted.
func dbServer(t *testing.T, redacted bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/databases":
			_, _ = w.Write([]byte(`{"data":[
				{"uuid":"db-1","name":"pg","engine":"postgres"},
				{"uuid":"db-2","name":"cache","engine":"redis"},
				{"uuid":"db-3","name":"weird","engine":"sqlite"}]}`))
		case "/api/v1/databases/db-1", "/api/v1/databases/db-2", "/api/v1/databases/db-3":
			if redacted {
				_, _ = w.Write([]byte(`{"engine":"postgres"}`))
				return
			}
			_, _ = w.Write([]byte(`{"username":"u","password":"p","database":"d"}`))
		case "/api/v1/applications":
			_, _ = w.Write([]byte(`{"data":[{"uuid":"app-1","name":"varuna"}]}`))
		case "/api/v1/applications/app-1/components":
			_, _ = w.Write([]byte(`{"data":[
				{"name":"web","is_database":false},
				{"name":"postgres","is_database":true,"database_engine":"postgres"},
				{"name":"broken","is_database":true,"database_engine":""}]}`))
		case "/api/v1/applications/app-1/envs":
			if redacted {
				_, _ = w.Write([]byte(`{"data":[{"key":"SERVICE_USER_POSTGRES","value":null}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[
				{"key":"SERVICE_USER_POSTGRES","value":"u"},
				{"key":"SERVICE_PASSWORD_POSTGRES","value":"p"}]}`))
		case "/api/v1/applications/app-1/previews":
			_, _ = w.Write([]byte(`{"data":[{"uuid":"pv-1","pr_id":8,"status":"active"}]}`))
		case "/api/v1/databases/db-1/port-forwards", "/api/v1/databases/db-2/port-forwards",
			"/api/v1/applications/app-1/port-forwards",
			"/api/v1/applications/app-1/previews/pv-1/port-forwards":
			// The mint fails: the test cares about everything before the tunnel.
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"code":"boom","message":"mint refused"}`))
		default:
			w.WriteHeader(500)
			_, _ = fmt.Fprintf(w, `{"code":"boom","message":"unexpected path %s"}`, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fakeClientBin drops an executable shell stub named bin on a fresh PATH, so
// the launch path runs without a real database client.
func fakeClientBin(t *testing.T, bin string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(filepath.Join(dir, bin), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
}

func TestDbErrors(t *testing.T) {
	srv := dbServer(t, false)

	t.Run("without a client", func(t *testing.T) {
		setupHome(t)
		if err := runCmd(dbCmd(), "db/pg"); err == nil {
			t.Fatal("expected a client error")
		}
	})

	run := func(t *testing.T, args ...string) error {
		t.Helper()
		setupContext(t, srv.URL)
		return runCmd(dbCmd(), args...)
	}

	t.Run("bad ref", func(t *testing.T) {
		if err := run(t, "nope"); err == nil {
			t.Fatal("expected a ref error")
		}
	})
	t.Run("pr on a database ref", func(t *testing.T) {
		if err := run(t, "db/pg", "--pr", "8"); err == nil || !strings.Contains(err.Error(), "--pr only applies") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("service ref unsupported", func(t *testing.T) {
		if err := run(t, "svc/stack"); err == nil || !strings.Contains(err.Error(), "db expects") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("app without component", func(t *testing.T) {
		if err := run(t, "app/varuna"); err == nil || !strings.Contains(err.Error(), "name the database with -c") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown database", func(t *testing.T) {
		if err := run(t, "db/ghost"); err == nil {
			t.Fatal("expected a resolve error")
		}
	})
	t.Run("unknown app", func(t *testing.T) {
		if err := run(t, "app/ghost", "-c", "postgres"); err == nil {
			t.Fatal("expected a resolve error")
		}
	})
	t.Run("unsupported engine", func(t *testing.T) {
		if err := run(t, "db/weird"); err == nil || !strings.Contains(err.Error(), `unsupported engine "sqlite"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown preview", func(t *testing.T) {
		if err := run(t, "app/varuna", "-c", "postgres", "--pr", "99"); err == nil || !strings.Contains(err.Error(), "no preview") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("component is not a database", func(t *testing.T) {
		if err := run(t, "app/varuna", "-c", "web"); err == nil || !strings.Contains(err.Error(), "not a database") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("component missing", func(t *testing.T) {
		if err := run(t, "app/varuna", "-c", "ghost"); err == nil || !strings.Contains(err.Error(), `no service "ghost"`) {
			t.Fatalf("err = %v", err)
		}
	})
}

// Credentials redacted (no read:sensitive): db still forwards the port and
// explains how to connect manually instead of failing.
func TestDbRedactedCredentialsForwardsOnly(t *testing.T) {
	srv := dbServer(t, true)
	setupContext(t, srv.URL)
	var err error
	_, errOut := captureOutput(t, func() {
		err = runCmd(dbCmd(), "db/pg")
	})
	// The port-forward mint fails in this fake, and that error is the outcome;
	// the redaction notice must have been printed before it.
	if err == nil || !strings.Contains(err.Error(), "mint refused") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut, "credentials are redacted") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// No client binary installed: same graceful degradation, different message.
func TestDbMissingClientBinaryForwardsOnly(t *testing.T) {
	srv := dbServer(t, false)
	setupContext(t, srv.URL)
	t.Setenv("PATH", t.TempDir()) // empty PATH dir: redis-cli is nowhere
	var err error
	_, errOut := captureOutput(t, func() {
		err = runCmd(dbCmd(), "db/cache")
	})
	if err == nil || !strings.Contains(err.Error(), "mint refused") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(errOut, "redis-cli not found locally") {
		t.Fatalf("stderr = %q", errOut)
	}
}

// The full launch path: credentials present, client binary found (a stub), the
// client runs and exits zero. The tunnel mint still fails, but the command's
// outcome is the client's, as it should be once the console has run.
func TestDbLaunchesTheClient(t *testing.T) {
	srv := dbServer(t, false)
	setupContext(t, srv.URL)
	fakeClientBin(t, "psql")
	var err error
	_, _ = captureOutput(t, func() {
		err = runCmd(dbCmd(), "db/pg")
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
}

// A compose database service in a PR preview: component engine and creds are
// looked up on the preview, and the mint path targets the preview instance.
func TestDbComposeServiceInPreview(t *testing.T) {
	srv := dbServer(t, false)
	setupContext(t, srv.URL)
	fakeClientBin(t, "psql")
	var err error
	_, _ = captureOutput(t, func() {
		err = runCmd(dbCmd(), "app/varuna", "-c", "postgres", "--pr", "8")
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestComponentCreds(t *testing.T) {
	srv := dbServer(t, false)
	setupContext(t, srv.URL)
	c, err := newClient("")
	if err != nil {
		t.Fatal(err)
	}
	detail := c.componentCreds(context.Background(), "app-1", "postgres", false)
	if detail["username"] != "u" || detail["password"] != "p" || detail["database"] != "u" {
		t.Fatalf("detail = %+v", detail)
	}
	// An API failure yields an empty map, never an error: creds are best-effort.
	detail = c.componentCreds(context.Background(), "ghost", "postgres", false)
	if len(detail) != 0 {
		t.Fatalf("detail = %+v", detail)
	}
}

func TestComponentEngineListError(t *testing.T) {
	srv := dbServer(t, false)
	setupContext(t, srv.URL)
	c, err := newClient("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.componentEngine(context.Background(), "ghost", "postgres"); err == nil {
		t.Fatal("expected the API error to surface")
	}
}

func TestComponentCredsRedacted(t *testing.T) {
	srv := dbServer(t, true)
	setupContext(t, srv.URL)
	c, err := newClient("")
	if err != nil {
		t.Fatal(err)
	}
	detail := c.componentCreds(context.Background(), "app-1", "postgres", false)
	if len(detail) != 0 {
		t.Fatalf("redacted values must not fill the detail: %+v", detail)
	}
}
