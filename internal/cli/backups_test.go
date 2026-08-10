package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// backupsServer serves one database, its plans and their executions, and
// records every write it receives.
func backupsServer(t *testing.T, plans string) (*httptest.Server, *[]string) {
	t.Helper()
	var posted []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/databases", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"uuid":"db-1","name":"pg-main","engine":"postgres"}]}`))
	})
	mux.HandleFunc("/api/v1/databases/db-1/backups", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(plans))
	})
	mux.HandleFunc("/api/v1/databases/db-1/backups/plan-daily/executions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[
			{"uuid":"ex-1","backup_plan_uuid":"plan-daily","database_uuid":"db-1","status":"succeeded",
			 "trigger":"scheduled","filename":"pg-main-2026-08-09.sql.gz","size_bytes":5242880,
			 "checksum":"sha256:abc","s3_uploaded":true,"created_at":"2026-08-09T02:00:00Z","finished_at":"2026-08-09T02:01:30Z"},
			{"uuid":"ex-2","backup_plan_uuid":"plan-daily","database_uuid":"db-1","status":"partial",
			 "trigger":"manual","filename":"pg-main-2026-08-08.sql.gz","size_bytes":5120,
			 "message":"s3 upload failed","created_at":"2026-08-08T02:00:00Z","finished_at":"2026-08-08T02:00:40Z"}
		]}`))
	})
	mux.HandleFunc("/api/v1/databases/db-1/backups/plan-weekly/executions", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[]}`))
	})
	mux.HandleFunc("/api/v1/databases/db-1/backups/plan-daily/execute", func(w http.ResponseWriter, r *http.Request) {
		posted = append(posted, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_uuid":"jb-1","status_url":"/jobs/jb-1","execution_uuid":"ex-9"}`))
	})
	mux.HandleFunc("/api/v1/databases/db-1/backups/plan-weekly/execute", func(w http.ResponseWriter, r *http.Request) {
		posted = append(posted, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"job_uuid":"jb-2","status_url":"/jobs/jb-2","execution_uuid":"ex-10"}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &posted
}

const onePlan = `{"data":[{"uuid":"plan-daily","frequency":"daily","enabled":true,"save_local":true,
	"save_s3":true,"next_run_at":"2026-08-10T02:00:00Z","last_execution_status":"succeeded",
	"drill_enabled":true,"version":3,"created_at":"2026-07-01T00:00:00Z"}]}`

const twoPlans = `{"data":[
	{"uuid":"plan-daily","frequency":"daily","enabled":true,"save_local":true,"save_s3":true,
	 "last_execution_status":"succeeded","version":3,"created_at":"2026-07-01T00:00:00Z"},
	{"uuid":"plan-weekly","frequency":"weekly","enabled":false,"save_s3":true,"s3_only":true,
	 "version":1,"created_at":"2026-07-01T00:00:00Z"}
]}`

// A plan says what is supposed to happen and an execution says what did: the
// listing prints both, because the question asked of a backup is the second one.
func TestBackupsListShowsPlansAndExecutions(t *testing.T) {
	srv, _ := backupsServer(t, onePlan)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(dbGroup(), "backups", "list", "pg-main") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{
		"daily", "enabled", "local+s3", "plan-daily", // the plan
		"succeeded", "scheduled", "5.0 MB", "pg-main-2026-08-09.sql.gz", "ex-1", // its history
		"partial", // never folded into success or failure (§20.5)
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want it to contain %q", out, want)
		}
	}
	// One plan needs no PLAN column: it would be the same uuid down the page.
	if strings.Contains(out, "PLAN") {
		t.Fatalf("stdout = %q — the plan column only earns its width with several plans", out)
	}
}

// With several plans the executions must say which plan produced them, or the
// history is one undifferentiated pile.
func TestBackupsListLabelsExecutionsWhenSeveralPlans(t *testing.T) {
	srv, _ := backupsServer(t, twoPlans)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(dbGroup(), "backups", "list", "pg-main") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"PLAN", "plan-daily", "plan-weekly", "s3 only", "disabled"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout = %q, want it to contain %q", out, want)
		}
	}
}

// `ls` is registered on every listing, and never shown in the help (ADR-070 §4).
func TestBackupsListAcceptsTheLsAlias(t *testing.T) {
	srv, _ := backupsServer(t, onePlan)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(dbGroup(), "backups", "ls", "pg-main") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "plan-daily") {
		t.Fatalf("stdout = %q", out)
	}
}

// -o json keeps the API objects whole: a script reads the retention rules, the
// drill flags and the checksum, none of which the table has a column for.
func TestBackupsListJSONPassesTheAPIObjectsThrough(t *testing.T) {
	srv, _ := backupsServer(t, onePlan)
	setupContext(t, srv.URL)
	flags.output = "json"

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(dbGroup(), "backups", "list", "pg-main") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	var got []struct {
		Plan       map[string]any   `json:"plan"`
		Executions []map[string]any `json:"executions"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v (%q)", err, out)
	}
	if len(got) != 1 || len(got[0].Executions) != 2 {
		t.Fatalf("got = %+v", got)
	}
	if got[0].Plan["drill_enabled"] != true || got[0].Plan["version"] != float64(3) {
		t.Fatalf("plan lost fields the table has no column for: %+v", got[0].Plan)
	}
	if got[0].Executions[0]["checksum"] != "sha256:abc" {
		t.Fatalf("execution lost its checksum: %+v", got[0].Executions[0])
	}
}

// -n bounds the history: a daily plan kept for a year is 365 rows nobody asked
// for.
func TestBackupsListRefusesANonPositiveLimit(t *testing.T) {
	srv, _ := backupsServer(t, onePlan)
	setupContext(t, srv.URL)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(dbGroup(), "backups", "list", "pg-main", "-n", "0") })
	if err == nil || !IsUsageError(err) {
		t.Fatalf("err = %v, want a usage error", err)
	}
}

// `run` on the only plan needs no --plan, and posts to that plan's execute path.
func TestBackupsRunPostsToExecute(t *testing.T) {
	srv, posted := backupsServer(t, onePlan)
	setupContext(t, srv.URL)

	var err error
	out, _ := captureOutput(t, func() { err = runCmd(dbGroup(), "backups", "run", "pg-main") })
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(*posted) != 1 || (*posted)[0] != "POST /api/v1/databases/db-1/backups/plan-daily/execute" {
		t.Fatalf("posted = %v", *posted)
	}
	if !strings.Contains(out, "ex-9") {
		t.Fatalf("stdout = %q — the created execution is what makes the trigger traceable", out)
	}
}

// Several plans differ in destination and retention: running "a" backup when
// the caller meant the S3 one produces a file nobody will look for. The refusal
// is a usage error and names the plans it is choosing between.
func TestBackupsRunRefusesAmbiguityWithoutPlan(t *testing.T) {
	srv, posted := backupsServer(t, twoPlans)
	setupContext(t, srv.URL)

	var err error
	_, _ = captureOutput(t, func() { err = runCmd(dbGroup(), "backups", "run", "pg-main") })
	if err == nil || !IsUsageError(err) {
		t.Fatalf("err = %v, want a usage error", err)
	}
	for _, want := range []string{"--plan plan-daily", "--plan plan-weekly", "daily", "weekly"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %q, want it to mention %q", err, want)
		}
	}
	if len(*posted) != 0 {
		t.Fatalf("posted = %v — an ambiguous run must trigger nothing", *posted)
	}
}

func TestBackupsRunWithPlan(t *testing.T) {
	t.Run("names the plan to run", func(t *testing.T) {
		srv, posted := backupsServer(t, twoPlans)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() {
			err = runCmd(dbGroup(), "backups", "run", "pg-main", "--plan", "plan-weekly")
		})
		if err != nil {
			t.Fatalf("err = %v", err)
		}
		if len(*posted) != 1 || (*posted)[0] != "POST /api/v1/databases/db-1/backups/plan-weekly/execute" {
			t.Fatalf("posted = %v", *posted)
		}
	})

	t.Run("an unknown plan lists the real ones", func(t *testing.T) {
		srv, posted := backupsServer(t, twoPlans)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() {
			err = runCmd(dbGroup(), "backups", "run", "pg-main", "--plan", "plan-hourly")
		})
		if err == nil || !IsUsageError(err) || !strings.Contains(err.Error(), "plan-weekly") {
			t.Fatalf("err = %v", err)
		}
		if len(*posted) != 0 {
			t.Fatalf("posted = %v", *posted)
		}
	})

	t.Run("no plan at all is the database's state, not the caller's spelling", func(t *testing.T) {
		srv, _ := backupsServer(t, `{"data":[]}`)
		setupContext(t, srv.URL)
		var err error
		_, _ = captureOutput(t, func() { err = runCmd(dbGroup(), "backups", "run", "pg-main") })
		if err == nil || !strings.Contains(err.Error(), "no backup plan") {
			t.Fatalf("err = %v", err)
		}
		if IsUsageError(err) {
			t.Fatalf("err = %v — nothing was mistyped, so this is not exit 2", err)
		}
	})
}

// The absence of `restore` is a DECISION (ADR-070 §2), not an oversight, and
// `download` has no endpoint to call at all (§1's gap list). Both are asserted
// so that adding either takes a deliberate edit to this test.
func TestBackupsHasNoRestoreOrDownloadVerb(t *testing.T) {
	verbs := map[string]bool{}
	for _, c := range backupsCmd().Commands() {
		verbs[c.Name()] = true
		for _, a := range c.Aliases {
			verbs[a] = true
		}
	}
	for _, forbidden := range []string{"restore", "download", "drill", "get"} {
		if verbs[forbidden] {
			t.Fatalf("`db backups %s` exists — its absence is a decision (ADR-070 §2)", forbidden)
		}
	}
	for _, want := range []string{"list", "run"} {
		if !verbs[want] {
			t.Fatalf("`db backups %s` is missing", want)
		}
	}
}

// The group is a database's, and only a database's: the same assertion from the
// other side, so a backup verb cannot drift onto an application.
func TestBackupsIsOnlyUnderTheDatabaseGroup(t *testing.T) {
	for _, group := range []*cobra.Command{appGroup(), svcGroup()} {
		for _, c := range group.Commands() {
			if c.Name() == "backups" {
				t.Fatalf("`akerdock %s backups` exists — only a database has backups", group.Name())
			}
		}
	}
}

func TestBackupSizeAndDestination(t *testing.T) {
	small, big := int64(512), int64(1610612736)
	if got := backupSize(nil); got != "-" {
		t.Errorf("backupSize(nil) = %q", got)
	}
	if got := backupSize(&small); got != "512 B" {
		t.Errorf("backupSize(512) = %q", got)
	}
	if got := backupSize(&big); got != "1.5 GB" {
		t.Errorf("backupSize(1.5GiB) = %q", got)
	}
	cases := []struct {
		plan backupPlan
		want string
	}{
		{backupPlan{SaveLocal: true, SaveS3: true}, "local+s3"},
		{backupPlan{SaveS3: true, S3Only: true}, "s3 only"},
		{backupPlan{SaveLocal: true}, "local"},
		{backupPlan{}, "-"},
	}
	for _, tc := range cases {
		if got := backupDestination(tc.plan); got != tc.want {
			t.Errorf("backupDestination(%+v) = %q, want %q", tc.plan, got, tc.want)
		}
	}
}

// A plan's timezone belongs on the line that says when it runs: "0 2 * * *"
// alone does not say which 2am, and that is where a nightly backup silently
// moves by an hour twice a year. UTC stays implicit, being the default.
func TestPlanScheduleFoldsTheTimezone(t *testing.T) {
	cases := []struct {
		plan backupPlan
		want string
	}{
		{backupPlan{Frequency: "0 2 * * *", Timezone: "Europe/Paris"}, "0 2 * * * (Europe/Paris)"},
		{backupPlan{Frequency: "daily", Timezone: "UTC"}, "daily"},
		{backupPlan{Frequency: "daily"}, "daily"},
	}
	for _, tc := range cases {
		if got := planSchedule(tc.plan); got != tc.want {
			t.Errorf("planSchedule(%+v) = %q, want %q", tc.plan, got, tc.want)
		}
	}
}
