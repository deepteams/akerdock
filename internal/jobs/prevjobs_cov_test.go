// Coverage-focused tests for the preview lifecycle jobs (previewpr,
// previewcommands, previewdestroy, previewfeedback, previewforge) and the
// webhook fan-out jobs (webhookprocess, githubapppush, githubappcomment).
//
// The prevjobsDB fake is a steerable pgx-protocol double: results are shaped
// per generated query name (the `-- name:` marker sqlc embeds in every SQL
// constant), so each test drives exactly the branch it targets. Forge and
// GitHub API traffic goes to loopback httptest servers.
package jobs

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	dockertypes "github.com/docker/docker/api/types"
	containertypes "github.com/docker/docker/api/types/container"
	imagetypes "github.com/docker/docker/api/types/image"
	networktypes "github.com/docker/docker/api/types/network"
	volumetypes "github.com/docker/docker/api/types/volume"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/agentwire"
	"github.com/deepteams/akerdock/internal/audit"
	dockerfake "github.com/deepteams/akerdock/internal/dockerruntime/fake"
	"github.com/deepteams/akerdock/internal/envelope"
	"github.com/deepteams/akerdock/internal/gitforge"
	"github.com/deepteams/akerdock/internal/gitwebhook"
	hostopsfake "github.com/deepteams/akerdock/internal/hostops/fake"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/store"
)

// ---------------------------------------------------------------------------
// Steerable DBTX fake
// ---------------------------------------------------------------------------

// prevjobsDB shapes every scan by generated query name. All maps are keyed by
// the sqlc query name (e.g. "GetPreviewByIdentity").
type prevjobsDB struct {
	blob   []byte // default fill for []byte columns
	truthy bool   // default fill for bool columns

	enums     map[string]string // enum TYPE name -> value (e.g. "PreviewStatus": "active")
	errs      map[string]error  // query -> error (Exec, Query, and row Scan)
	rows      map[string]int    // query -> row count for Query (default 1)
	fillPtr   map[string]bool   // query -> fill nullable columns instead of NULL
	strs      map[string]string // query -> value for every string column
	ints      map[string]int64  // query -> value for every integer column
	bools     map[string]bool   // query -> value for every bool column
	blobs     map[string][]byte // query -> value for []byte columns (nil = SQL NULL)
	tsInvalid map[string]bool   // query -> timestamps scan as NULL
}

func prevjobsQueryName(sql string) string {
	_, after, ok := strings.Cut(sql, "-- name: ")
	if !ok {
		return ""
	}
	name, _, _ := strings.Cut(after, " ")
	return name
}

func (db *prevjobsDB) Exec(_ context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	if err := db.errs[prevjobsQueryName(sql)]; err != nil {
		return pgconn.CommandTag{}, err
	}
	return pgconn.NewCommandTag("UPDATE 1"), nil
}

func (db *prevjobsDB) Query(_ context.Context, sql string, _ ...any) (pgx.Rows, error) {
	name := prevjobsQueryName(sql)
	if err := db.errs[name]; err != nil {
		return nil, err
	}
	remaining := 1
	if n, ok := db.rows[name]; ok {
		remaining = n
	}
	return &prevjobsRows{db: db, name: name, remaining: remaining}, nil
}

func (db *prevjobsDB) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	return prevjobsRow{db: db, name: prevjobsQueryName(sql)}
}

func (db *prevjobsDB) fill(name string, dest any) error {
	switch d := dest.(type) {
	case *pgtype.Timestamptz:
		if db.tsInvalid[name] {
			*d = pgtype.Timestamptz{}
			return nil
		}
	case *pgtype.Timestamp:
		if db.tsInvalid[name] {
			*d = pgtype.Timestamp{}
			return nil
		}
	case *[]byte:
		if b, ok := db.blobs[name]; ok {
			if b == nil {
				*d = nil
			} else {
				*d = append([]byte(nil), b...)
			}
			return nil
		}
	}
	v := reflect.ValueOf(dest)
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		e := v.Elem()
		switch e.Kind() {
		case reflect.Pointer:
			if db.fillPtr[name] {
				n := reflect.New(e.Type().Elem())
				if err := db.fill(name, n.Interface()); err != nil {
					return err
				}
				e.Set(n)
			} else {
				e.SetZero()
			}
			return nil
		case reflect.String:
			if e.Type().Name() != "string" {
				if val, ok := db.enums[e.Type().Name()]; ok {
					e.SetString(val)
					return nil
				}
			} else if s, ok := db.strs[name]; ok {
				e.SetString(s)
				return nil
			}
		case reflect.Bool:
			if b, ok := db.bools[name]; ok {
				e.SetBool(b)
				return nil
			}
		case reflect.Int, reflect.Int16, reflect.Int32, reflect.Int64:
			if n, ok := db.ints[name]; ok {
				e.SetInt(n)
				return nil
			}
		}
	}
	return fillJobDestination(dest, db.blob, db.truthy)
}

type prevjobsRow struct {
	db   *prevjobsDB
	name string
}

func (r prevjobsRow) Scan(dest ...any) error {
	if err := r.db.errs[r.name]; err != nil {
		return err
	}
	for _, d := range dest {
		if err := r.db.fill(r.name, d); err != nil {
			return err
		}
	}
	return nil
}

type prevjobsRows struct {
	db        *prevjobsDB
	name      string
	remaining int
	current   bool
}

func (r *prevjobsRows) Close()                                     { r.remaining = 0 }
func (*prevjobsRows) Err() error                                   { return nil }
func (*prevjobsRows) CommandTag() pgconn.CommandTag                { return pgconn.NewCommandTag("SELECT 1") }
func (*prevjobsRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (*prevjobsRows) Values() ([]any, error)                       { return nil, nil }
func (*prevjobsRows) RawValues() [][]byte                          { return nil }
func (*prevjobsRows) Conn() *pgx.Conn                              { return nil }

func (r *prevjobsRows) Next() bool {
	if r.remaining == 0 {
		r.current = false
		return false
	}
	r.remaining--
	r.current = true
	return true
}

func (r *prevjobsRows) Scan(dest ...any) error {
	if !r.current {
		return errors.New("Scan called before Next")
	}
	for _, d := range dest {
		if err := r.db.fill(r.name, d); err != nil {
			return err
		}
	}
	return nil
}

var (
	_ store.DBTX = (*prevjobsDB)(nil)
	_ pgx.Rows   = (*prevjobsRows)(nil)
)

// ---------------------------------------------------------------------------
// Shared fixtures
// ---------------------------------------------------------------------------

func prevjobsDeps(t *testing.T) (*store.Queries, *envelope.Keyring, *slog.Logger, *prevjobsDB) {
	t.Helper()
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	keyring, err := envelope.Parse([]byte("1:" + key + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	db := &prevjobsDB{
		blob:      []byte("prevjobs: neither JSON nor ciphertext"),
		truthy:    true,
		enums:     map[string]string{},
		errs:      map[string]error{},
		rows:      map[string]int{},
		fillPtr:   map[string]bool{},
		strs:      map[string]string{},
		ints:      map[string]int64{},
		bools:     map[string]bool{},
		blobs:     map[string][]byte{},
		tsInvalid: map[string]bool{},
	}
	return store.New(db), keyring, slog.New(slog.NewTextHandler(io.Discard, nil)), db
}

func prevjobsApp(mut func(*store.GetApplicationByIDRow)) store.GetApplicationByIDRow {
	var app store.GetApplicationByIDRow
	app.Resource.ID = 1
	app.Resource.TeamID = 1
	app.Resource.DestinationID = 1
	app.Resource.Name = "unit-app"
	_ = app.Resource.Uuid.Scan(jobFixtureUUID)
	app.Application.PreviewsEnabled = true
	app.Application.PreviewDeployOnOpen = true
	app.Application.PreviewCommentCommandsEnabled = true
	if mut != nil {
		mut(&app)
	}
	return app
}

func prevjobsPREvent(action string) gitwebhook.PullRequestEvent {
	return gitwebhook.PullRequestEvent{
		Action: action, Number: 7, HeadRef: "feature", HeadSHA: "cafe1234",
		RepoReference: "deep/repo",
	}
}

func prevjobsCommentEvent(body string) gitwebhook.CommentEvent {
	return gitwebhook.CommentEvent{
		Number: 7, Body: body, AuthorUsername: "dev", AuthorID: 9,
		RepoReference: "deep/repo", OnPullRequest: true,
	}
}

func prevjobsPRJSON(action string, headRepoID int64, draft bool, labels ...string) []byte {
	ls := make([]string, 0, len(labels))
	for _, l := range labels {
		ls = append(ls, fmt.Sprintf(`{"name":%q}`, l))
	}
	return fmt.Appendf(nil,
		`{"action":%q,"number":7,"pull_request":{"number":7,"draft":%t,"head":{"ref":"feature","sha":"cafe1234","repo":{"id":%d}},"base":{"repo":{"id":42,"full_name":"deep/repo"}},"labels":[%s]},"repository":{"full_name":"deep/repo"}}`,
		action, draft, headRepoID, strings.Join(ls, ","))
}

func prevjobsPushJSON(ref, after string) []byte {
	return fmt.Appendf(nil,
		`{"ref":%q,"after":%q,"head_commit":{"message":"feat: x"},"commits":[{"added":["src/a.go"]}]}`,
		ref, after)
}

func prevjobsCommentJSON(body string) []byte {
	return fmt.Appendf(nil,
		`{"action":"created","issue":{"number":7,"pull_request":{}},"comment":{"body":%q,"user":{"id":9,"login":"dev"}},"repository":{"full_name":"deep/repo"}}`,
		body)
}

var prevjobsGitlabNoteJSON = []byte(`{"object_kind":"note","user":{"id":9,"username":"dev"},"project":{"id":31},"object_attributes":{"note":"/keep","notable_type":"MergeRequest"},"merge_request":{"iid":7}}`)

func prevjobsEncrypt(t *testing.T, keyring *envelope.Keyring, table, column string, value []byte) []byte {
	t.Helper()
	enc, err := keyring.Encrypt(table, column, jobFixtureUUID, value)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

var (
	prevjobsRSAOnce sync.Once
	prevjobsRSAPEM  []byte
	prevjobsRSAErr  error //nolint:errname // Coverage-suite globals keep a collision-resistant prefix.
)

func prevjobsRSAKeyPEM(t *testing.T) []byte {
	t.Helper()
	prevjobsRSAOnce.Do(func() {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			prevjobsRSAErr = err
			return
		}
		prevjobsRSAPEM = pem.EncodeToMemory(&pem.Block{
			Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
		})
	})
	if prevjobsRSAErr != nil {
		t.Fatal(prevjobsRSAErr)
	}
	return prevjobsRSAPEM
}

// prevjobsGithubServer answers the GitHub API calls the feedback and rights
// paths make. fail maps a path substring to an HTTP status to force errors.
func prevjobsGithubServer(t *testing.T, fail map[string]int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for sub, code := range fail {
			if strings.Contains(r.URL.Path, sub) {
				w.WriteHeader(code)
				_, _ = w.Write([]byte(`{}`))
				return
			}
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/access_tokens"):
			fmt.Fprintf(w, `{"token":"tok","expires_at":%q}`,
				time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
		case strings.HasSuffix(r.URL.Path, "/permission"):
			fmt.Fprint(w, `{"permission":"write"}`)
		case strings.Contains(r.URL.Path, "/comments") && r.Method == http.MethodGet:
			fmt.Fprint(w, `[]`)
		case strings.HasSuffix(r.URL.Path, "/deployments"):
			fmt.Fprint(w, `{"id":7}`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// prevjobsGitlabServer answers the GitLab API: membership at the given access
// level, an empty notes list, everything else 200 {}.
func prevjobsGitlabServer(t *testing.T, accessLevel int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/members/all/"):
			fmt.Fprintf(w, `{"access_level":%d}`, accessLevel)
		case strings.Contains(r.URL.Path, "/notes") && r.Method == http.MethodGet:
			fmt.Fprint(w, `[]`)
		default:
			fmt.Fprint(w, `{}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// ---------------------------------------------------------------------------
// previewpr.go — HandlePreviewPREvent
// ---------------------------------------------------------------------------

func TestPrevjobsHandlePREventClosed(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(db *prevjobsDB)
		want    string
		wantErr bool
	}{
		{
			name:  "no preview row is a no-op",
			setup: func(db *prevjobsDB) { db.errs["GetPreviewByIdentity"] = pgx.ErrNoRows },
			want:  "no preview to destroy",
		},
		{
			name:  "already destroyed",
			setup: func(db *prevjobsDB) { db.enums["PreviewStatus"] = "destroyed" },
			want:  "already destroyed",
		},
		{
			name:  "live preview queues a destroy",
			setup: func(db *prevjobsDB) { db.enums["PreviewStatus"] = "active" },
			want:  "destroy queued",
		},
		{
			name: "enqueue failure propagates",
			setup: func(db *prevjobsDB) {
				db.enums["PreviewStatus"] = "active"
				db.errs["EnqueueJob"] = errors.New("queue down")
			},
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, keyring, logger, db := prevjobsDeps(t)
			tc.setup(db)
			outcome, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
				prevjobsApp(nil), store.GitProviderGithub, prevjobsPREvent("closed"))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("outcome = %q, want error", outcome)
				}
				return
			}
			if err != nil || outcome != tc.want {
				t.Fatalf("outcome = %q, err = %v, want %q", outcome, err, tc.want)
			}
		})
	}
}

func TestPrevjobsHandlePREventLabelPolicy(t *testing.T) {
	t.Run("required label absent destroys a live preview", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active"
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewRequireLabel = ptr("preview")
		})
		outcome, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			app, store.GitProviderGithub, prevjobsPREvent("unlabeled"))
		if err != nil || outcome != "required label removed: destroy queued" {
			t.Fatalf("outcome = %q, err = %v", outcome, err)
		}
	})
	t.Run("required label absent with no preview reports the gate", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["GetPreviewByIdentity"] = pgx.ErrNoRows
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewRequireLabel = ptr("preview")
		})
		outcome, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			app, store.GitProviderGithub, prevjobsPREvent("opened"))
		if err != nil || outcome != "label preview required (preview_require_label)" {
			t.Fatalf("outcome = %q, err = %v", outcome, err)
		}
	})
	t.Run("destroy error under the label gate propagates", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active"
		db.errs["EnqueueJob"] = errors.New("queue down")
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewRequireLabel = ptr("preview")
		})
		if _, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			app, store.GitProviderGithub, prevjobsPREvent("unlabeled")); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("label events without a configured label are noise", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		outcome, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			prevjobsApp(nil), store.GitProviderGithub, prevjobsPREvent("labeled"))
		if err != nil || outcome != "label events ignored without preview_require_label" {
			t.Fatalf("outcome = %q, err = %v", outcome, err)
		}
	})
	t.Run("labeled preview already serving this SHA is left alone", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active"
		db.fillPtr["UpsertPreview"] = true
		db.strs["UpsertPreview"] = "cafe1234"
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewRequireLabel = ptr("preview")
		})
		event := prevjobsPREvent("labeled")
		event.Labels = []string{"preview"}
		outcome, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			app, store.GitProviderGithub, event)
		if err != nil || outcome != "already deployed at this SHA" {
			t.Fatalf("outcome = %q, err = %v", outcome, err)
		}
	})
}

func TestPrevjobsHandlePREventGates(t *testing.T) {
	t.Run("draft excluded", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewExcludeDrafts = true
		})
		event := prevjobsPREvent("opened")
		event.Draft = true
		outcome, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			app, store.GitProviderGithub, event)
		if err != nil || outcome != "draft excluded (preview_exclude_drafts)" {
			t.Fatalf("outcome = %q, err = %v", outcome, err)
		}
	})
	t.Run("fork ignored without the approval flow", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		event := prevjobsPREvent("opened")
		event.IsFork = true
		outcome, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			prevjobsApp(nil), store.GitProviderGithub, event)
		if err != nil || !strings.Contains(outcome, "fork ignored") {
			t.Fatalf("outcome = %q, err = %v", outcome, err)
		}
	})
	t.Run("fork waits for the maintainer approval", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "queued"
		db.tsInvalid["UpsertPreview"] = true // fork_approved_at IS NULL
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewForkApprovalEnabled = true
		})
		event := prevjobsPREvent("opened")
		event.IsFork = true
		outcome, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			app, store.GitProviderGithub, event)
		if err != nil || outcome != "fork waiting for maintainer approval (INV-010)" {
			t.Fatalf("outcome = %q, err = %v", outcome, err)
		}
	})
	t.Run("upsert failure propagates", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["UpsertPreview"] = errors.New("constraint violated")
		if _, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			prevjobsApp(nil), store.GitProviderGithub, prevjobsPREvent("opened")); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("unknown action is reported, not deployed", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		outcome, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			prevjobsApp(nil), store.GitProviderGithub, prevjobsPREvent("edited"))
		if err != nil || outcome != "action edited not handled" {
			t.Fatalf("outcome = %q, err = %v", outcome, err)
		}
	})
	t.Run("manual-first reservation waits for a human", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "queued"
		db.tsInvalid["UpsertPreview"] = true
		db.bools["UpsertPreview"] = false // not a fork, never deployed
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewDeployOnOpen = false
		})
		outcome, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			app, store.GitProviderGithub, prevjobsPREvent("opened"))
		if err != nil || outcome != "awaiting manual deploy (preview_deploy_on_open=false)" {
			t.Fatalf("outcome = %q, err = %v", outcome, err)
		}
	})
	t.Run("capacity cap queues instead of deploying", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "queued"
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewMaxConcurrent = ptr(int32(1))
		})
		outcome, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			app, store.GitProviderGithub, prevjobsPREvent("opened"))
		if err != nil || !strings.HasPrefix(outcome, "queued: concurrency cap reached") {
			t.Fatalf("outcome = %q, err = %v", outcome, err)
		}
	})
	t.Run("scaffolding failure propagates", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "queued"
		db.errs["CreateGeneratedPreviewEnvVar"] = errors.New("insert refused")
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewProtection = store.PreviewProtectionBasicAuth
		})
		if _, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			app, store.GitProviderGithub, prevjobsPREvent("opened")); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("promotion failure propagates", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "queued"
		db.errs["CreateDeployment"] = errors.New("create refused")
		if _, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			prevjobsApp(nil), store.GitProviderGithub, prevjobsPREvent("opened")); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("promotion queues the deployment", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "queued"
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewCancelObsoleteBuilds = true
		})
		outcome, err := HandlePreviewPREvent(context.Background(), q, keyring, logger,
			app, store.GitProviderGithub, prevjobsPREvent("synchronize"))
		if err != nil || outcome != "deployment queued" {
			t.Fatalf("outcome = %q, err = %v", outcome, err)
		}
	})
}

// ---------------------------------------------------------------------------
// previewpr.go — scaffolding, FQDN resolution, promotion
// ---------------------------------------------------------------------------

func TestPrevjobsEnsurePreviewScaffolding(t *testing.T) {
	t.Run("generates the slug and the templated FQDN once", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.strs["ListDomainsForApplication"] = "apps.example"
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewUrlTemplate = "pr-{{pr_id}}.{{domain}}"
		})
		preview := store.Preview{ID: 3, PrID: 7}
		if err := ensurePreviewScaffolding(context.Background(), q, keyring, app, &preview); err != nil {
			t.Fatal(err)
		}
		if preview.RandomSlug == nil || *preview.RandomSlug == "" {
			t.Fatal("random slug not generated")
		}
		if preview.Fqdn == nil || *preview.Fqdn != "pr-7.apps.example" {
			t.Fatalf("fqdn = %v", preview.Fqdn)
		}
	})
	t.Run("existing slug and fqdn are kept", func(t *testing.T) {
		q, keyring, _, _ := prevjobsDeps(t)
		preview := store.Preview{ID: 3, PrID: 7, RandomSlug: ptr("abc123"), Fqdn: ptr("pr-7.old.example")}
		if err := ensurePreviewScaffolding(context.Background(), q, keyring, prevjobsApp(nil), &preview); err != nil {
			t.Fatal(err)
		}
		if *preview.Fqdn != "pr-7.old.example" || *preview.RandomSlug != "abc123" {
			t.Fatalf("preview mutated: %+v", preview)
		}
	})
	t.Run("basic auth generates the shared credential", func(t *testing.T) {
		q, keyring, _, _ := prevjobsDeps(t)
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewProtection = store.PreviewProtectionBasicAuth
		})
		preview := store.Preview{ID: 3, PrID: 7, RandomSlug: ptr("abc"), Fqdn: ptr("x.example")}
		if err := ensurePreviewScaffolding(context.Background(), q, keyring, app, &preview); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("basic auth persistence failure propagates", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.errs["CreateGeneratedPreviewEnvVar"] = errors.New("insert refused")
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewProtection = store.PreviewProtectionBasicAuth
		})
		preview := store.Preview{ID: 3, PrID: 7, RandomSlug: ptr("abc"), Fqdn: ptr("x.example")}
		if err := ensurePreviewScaffolding(context.Background(), q, keyring, app, &preview); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestPrevjobsPreviewFQDN(t *testing.T) {
	base := func(mut func(*store.GetApplicationByIDRow)) store.GetApplicationByIDRow {
		return prevjobsApp(mut)
	}
	t.Run("no template falls back to the server wildcard", func(t *testing.T) {
		q, _, _, db := prevjobsDeps(t)
		db.fillPtr["GetServerByID"] = true
		db.strs["GetServerByID"] = "wild.example"
		fqdn, err := previewFQDN(context.Background(), q, base(nil), 7, "r4nd0m")
		if err != nil || fqdn != "pr-7-33333333.wild.example" {
			t.Fatalf("fqdn = %q, err = %v", fqdn, err)
		}
	})
	t.Run("no wildcard means an unrouted preview", func(t *testing.T) {
		q, _, _, _ := prevjobsDeps(t)
		fqdn, err := previewFQDN(context.Background(), q, base(nil), 7, "")
		if err != nil || fqdn != "" {
			t.Fatalf("fqdn = %q, err = %v", fqdn, err)
		}
	})
	t.Run("destination lookup failure propagates", func(t *testing.T) {
		q, _, _, db := prevjobsDeps(t)
		db.errs["GetDestinationByID"] = errors.New("destination gone")
		if _, err := previewFQDN(context.Background(), q, base(nil), 7, ""); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("server lookup failure propagates", func(t *testing.T) {
		q, _, _, db := prevjobsDeps(t)
		db.errs["GetServerByID"] = errors.New("server gone")
		if _, err := previewFQDN(context.Background(), q, base(nil), 7, ""); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("explicit template resolves with the first domain", func(t *testing.T) {
		q, _, _, db := prevjobsDeps(t)
		db.strs["ListDomainsForApplication"] = "apps.example"
		app := base(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewUrlTemplate = "pr-{{pr_id}}-{{random}}.{{domain}}"
		})
		fqdn, err := previewFQDN(context.Background(), q, app, 7, "r4nd")
		if err != nil || fqdn != "pr-7-r4nd.apps.example" {
			t.Fatalf("fqdn = %q, err = %v", fqdn, err)
		}
	})
	t.Run("domain template without a domain falls back to the wildcard", func(t *testing.T) {
		q, _, _, db := prevjobsDeps(t)
		db.rows["ListDomainsForApplication"] = 0
		app := base(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewUrlTemplate = "pr-{{pr_id}}.{{domain}}"
		})
		fqdn, err := previewFQDN(context.Background(), q, app, 7, "")
		if err != nil || fqdn != "" {
			t.Fatalf("fqdn = %q, err = %v", fqdn, err)
		}
	})
	t.Run("service template resolves the web service", func(t *testing.T) {
		q, _, _, db := prevjobsDeps(t)
		db.fillPtr["ListServiceComponents"] = true
		app := base(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewUrlTemplates = []byte(`[{"host":"{{service}}-pr-{{pr_id}}.x.example"}]`)
		})
		fqdn, err := previewFQDN(context.Background(), q, app, 7, "")
		if err != nil || fqdn != "unit-pr-7.x.example" {
			t.Fatalf("fqdn = %q, err = %v", fqdn, err)
		}
	})
}

func TestPrevjobsPrimaryServiceName(t *testing.T) {
	t.Run("list failure yields no service", func(t *testing.T) {
		q, _, _, db := prevjobsDeps(t)
		db.errs["ListServiceComponents"] = errors.New("db gone")
		if got := primaryServiceName(context.Background(), q, prevjobsApp(nil)); got != "" {
			t.Fatalf("service = %q", got)
		}
	})
	t.Run("unroutable components fall back to the first one", func(t *testing.T) {
		q, _, _, _ := prevjobsDeps(t)
		if got := primaryServiceName(context.Background(), q, prevjobsApp(nil)); got != "unit" {
			t.Fatalf("service = %q", got)
		}
	})
	t.Run("no components at all yields no service", func(t *testing.T) {
		q, _, _, db := prevjobsDeps(t)
		db.rows["ListServiceComponents"] = 0
		if got := primaryServiceName(context.Background(), q, prevjobsApp(nil)); got != "" {
			t.Fatalf("service = %q", got)
		}
	})
}

func TestPrevjobsPreviewRandomWithoutSlug(t *testing.T) {
	if got := previewRandom(&store.Preview{}); got != "" {
		t.Fatalf("previewRandom = %q", got)
	}
}

func TestPrevjobsPromotePreviewDeployment(t *testing.T) {
	preview := store.Preview{ID: 4, PrID: 7}
	_ = preview.Uuid.Scan(jobFixtureUUID)

	t.Run("success without cancel-obsolete", func(t *testing.T) {
		q, _, logger, _ := prevjobsDeps(t)
		deployment, promoted, reason, err := PromotePreviewDeployment(context.Background(), q, logger,
			prevjobsApp(nil), preview, false)
		if err != nil || !promoted || reason != "" || deployment.ID == 0 {
			t.Fatalf("promoted = %v, reason = %q, err = %v", promoted, reason, err)
		}
	})
	t.Run("cancel-obsolete with nothing to cancel skips the log", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.rows["SupersedeObsoletePreviewDeployments"] = 0
		db.rows["ListCancellablePreviewDeploymentIDs"] = 0
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewCancelObsoleteBuilds = true
		})
		_, promoted, _, err := PromotePreviewDeployment(context.Background(), q, logger, app, preview, true)
		if err != nil || !promoted {
			t.Fatalf("promoted = %v, err = %v", promoted, err)
		}
	})

	fail := func(name string, mut func(*store.GetApplicationByIDRow)) func(t *testing.T) {
		return func(t *testing.T) {
			q, _, logger, db := prevjobsDeps(t)
			db.errs[name] = errors.New(name + " failed")
			app := prevjobsApp(mut)
			_, promoted, _, err := PromotePreviewDeployment(context.Background(), q, logger, app, preview, false)
			if err == nil || promoted {
				t.Fatalf("want %s error, got promoted=%v err=%v", name, promoted, err)
			}
		}
	}
	withCap := func(a *store.GetApplicationByIDRow) { a.Application.PreviewMaxConcurrent = ptr(int32(5)) }
	withCancel := func(a *store.GetApplicationByIDRow) { a.Application.PreviewCancelObsoleteBuilds = true }
	t.Run("count failure", fail("CountLivePreviewsForApplication", withCap))
	t.Run("destination failure", fail("GetDestinationByID", nil))
	t.Run("create failure", fail("CreateDeployment", nil))
	t.Run("supersede failure", fail("SupersedeObsoletePreviewDeployments", withCancel))
	t.Run("cancel jobs failure", fail("CancelJobsForDeployments", withCancel))
	t.Run("list cancellable failure", fail("ListCancellablePreviewDeploymentIDs", withCancel))
	t.Run("request cancel failure", fail("RequestDeploymentJobCancel", withCancel))
	t.Run("enqueue failure", fail("EnqueueJob", nil))
}

// ---------------------------------------------------------------------------
// previewpr.go — GithubAppPullRequest.Execute
// ---------------------------------------------------------------------------

func TestPrevjobsGithubAppPullRequestExecute(t *testing.T) {
	run := func(t *testing.T, db *prevjobsDB, q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger) (map[string]any, error) {
		t.Helper()
		j := store.Job{
			ID: 21, JobType: TypeGithubAppPullRequest,
			Payload: []byte(`{"delivery_id":1,"github_app_id":1}`),
		}
		h := &GithubAppPullRequest{Store: q, Keyring: keyring, Logger: logger}
		out, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j))
		if out == nil {
			return nil, err
		}
		return out.(map[string]any), err
	}
	prime := func(db *prevjobsDB, payload []byte) {
		db.blobs["GetWebhookDeliveryByID"] = payload
		db.enums["PreviewStatus"] = "queued"
	}

	t.Run("invalid payload", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		j := store.Job{ID: 21, JobType: TypeGithubAppPullRequest, Payload: []byte(`{`)}
		if _, err := (&GithubAppPullRequest{Store: q, Keyring: keyring, Logger: logger}).
			Execute(context.Background(), j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("missing delivery", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["GetWebhookDeliveryByID"] = pgx.ErrNoRows
		if _, err := run(t, db, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("unverified delivery refused", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.bools["GetWebhookDeliveryByID"] = false
		if _, err := run(t, db, q, keyring, logger); err == nil ||
			!strings.Contains(err.Error(), "unverified") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("fan-out deploys the bound application", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsPRJSON("opened", 42, false))
		out, err := run(t, db, q, keyring, logger)
		if err != nil {
			t.Fatal(err)
		}
		apps := out["applications"].(map[string]string)
		if apps[jobFixtureUUID] != "deployment queued" {
			t.Fatalf("applications = %#v", apps)
		}
	})
	t.Run("previews disabled is recorded per application", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsPRJSON("opened", 42, false))
		db.bools["GetApplicationByID"] = false
		out, err := run(t, db, q, keyring, logger)
		if err != nil {
			t.Fatal(err)
		}
		if out["applications"].(map[string]string)[jobFixtureUUID] != "previews disabled" {
			t.Fatalf("applications = %#v", out["applications"])
		}
	})
	t.Run("handler failure is recorded per application", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsPRJSON("opened", 42, false))
		db.errs["UpsertPreview"] = errors.New("upsert refused")
		out, err := run(t, db, q, keyring, logger)
		if err != nil {
			t.Fatal(err)
		}
		if got := out["applications"].(map[string]string)[jobFixtureUUID]; !strings.HasPrefix(got, "failed: ") {
			t.Fatalf("applications = %#v", out["applications"])
		}
	})
	t.Run("no bound application finishes ignored", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsPRJSON("opened", 42, false))
		db.rows["ListApplicationIDsForRepositoryPush"] = 0
		out, err := run(t, db, q, keyring, logger)
		if err != nil || len(out["applications"].(map[string]string)) != 0 {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("vanished application is skipped", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsPRJSON("opened", 42, false))
		db.errs["GetApplicationByID"] = pgx.ErrNoRows
		out, err := run(t, db, q, keyring, logger)
		if err != nil || len(out["applications"].(map[string]string)) != 0 {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("application list failure propagates", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsPRJSON("opened", 42, false))
		db.errs["ListApplicationIDsForRepositoryPush"] = errors.New("db gone")
		if _, err := run(t, db, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
}

// ---------------------------------------------------------------------------
// previewcommands.go
// ---------------------------------------------------------------------------

func prevjobsAllowRights(context.Context, string) (bool, error) { return true, nil }

func TestPrevjobsHandlePreviewCommentCore(t *testing.T) {
	type tc struct {
		name     string
		body     string
		mutApp   func(*store.GetApplicationByIDRow)
		mutEvent func(*gitwebhook.CommentEvent)
		setup    func(db *prevjobsDB)
		rights   func(context.Context, string) (bool, error)
		ignored  string
		accepted string
		wantErr  string
	}
	active := func(db *prevjobsDB) { db.enums["PreviewStatus"] = "active" }
	cases := []tc{
		{
			name: "not on a pull request", body: "/deploy",
			mutEvent: func(e *gitwebhook.CommentEvent) { e.OnPullRequest = false },
			ignored:  "not a comment on a pull request",
		},
		{
			name: "no command in the first line", body: "great work!",
			ignored: "no command in the first line",
		},
		{
			name: "comment commands disabled", body: "/deploy",
			mutApp:  func(a *store.GetApplicationByIDRow) { a.Application.PreviewCommentCommandsEnabled = false },
			ignored: "comment commands disabled (preview_comment_commands_enabled)",
		},
		{
			name: "previews disabled", body: "/deploy",
			mutApp:  func(a *store.GetApplicationByIDRow) { a.Application.PreviewsEnabled = false },
			ignored: "previews disabled",
		},
		{
			name: "unknown PR has no preview", body: "/deploy",
			setup:   func(db *prevjobsDB) { db.errs["GetPreviewByIdentity"] = pgx.ErrNoRows },
			ignored: "no preview known for this PR",
		},
		{
			name: "no credential refuses the command", body: "/deploy",
			setup: active, rights: nil,
			ignored: "no_api_credentials: comment commands need a provider API token",
		},
		{
			name: "rights check failure", body: "/deploy",
			setup:   active,
			rights:  func(context.Context, string) (bool, error) { return false, errors.New("api down") },
			wantErr: "author rights check failed",
		},
		{
			name: "author without write access", body: "/deploy",
			setup:   active,
			rights:  func(context.Context, string) (bool, error) { return false, nil },
			ignored: "author lacks write access",
		},
		{
			name: "destroy on a destroyed preview", body: "/destroy",
			setup:   func(db *prevjobsDB) { db.enums["PreviewStatus"] = "destroyed" },
			rights:  prevjobsAllowRights,
			ignored: "preview already destroyed",
		},
		{
			name: "destroy queues the teardown", body: "/destroy",
			setup: active, rights: prevjobsAllowRights,
			accepted: "destroy queued by /destroy",
		},
		{
			name: "destroy enqueue failure", body: "/destroy",
			setup: func(db *prevjobsDB) {
				active(db)
				db.errs["EnqueueJob"] = errors.New("queue down")
			},
			rights:  prevjobsAllowRights,
			wantErr: "queue down",
		},
		{
			name: "deploy on an unapproved fork", body: "/deploy",
			setup: func(db *prevjobsDB) {
				active(db)
				db.tsInvalid["GetPreviewByIdentity"] = true // fork_approved_at NULL
			},
			rights:  prevjobsAllowRights,
			ignored: "fork waiting for maintainer approval (INV-010)",
		},
		{
			name: "deploy upsert failure", body: "/deploy",
			setup: func(db *prevjobsDB) {
				active(db)
				db.bools["GetPreviewByIdentity"] = false // not a fork
				db.errs["UpsertPreview"] = errors.New("upsert refused")
			},
			rights:  prevjobsAllowRights,
			wantErr: "upsert refused",
		},
		{
			name: "deploy scaffolding failure", body: "/deploy",
			mutApp: func(a *store.GetApplicationByIDRow) {
				a.Application.PreviewProtection = store.PreviewProtectionBasicAuth
			},
			setup: func(db *prevjobsDB) {
				active(db)
				db.bools["GetPreviewByIdentity"] = false
				db.errs["CreateGeneratedPreviewEnvVar"] = errors.New("insert refused")
			},
			rights:  prevjobsAllowRights,
			wantErr: "insert refused",
		},
		{
			name: "deploy promotion failure", body: "/deploy",
			setup: func(db *prevjobsDB) {
				active(db)
				db.bools["GetPreviewByIdentity"] = false
				db.errs["CreateDeployment"] = errors.New("create refused")
			},
			rights:  prevjobsAllowRights,
			wantErr: "create refused",
		},
		{
			name: "deploy queued at the capacity cap", body: "/deploy",
			mutApp: func(a *store.GetApplicationByIDRow) {
				a.Application.PreviewMaxConcurrent = ptr(int32(1))
			},
			setup: func(db *prevjobsDB) {
				active(db)
				db.bools["GetPreviewByIdentity"] = false
			},
			rights:   prevjobsAllowRights,
			accepted: "queued by /deploy: concurrency cap reached (1 live)",
		},
		{
			name: "deploy promotes the preview", body: "/deploy",
			setup: func(db *prevjobsDB) {
				active(db)
				db.bools["GetPreviewByIdentity"] = false
			},
			rights:   prevjobsAllowRights,
			accepted: "deployment queued by /deploy",
		},
		{
			name: "rebuild on an unapproved fork", body: "/rebuild",
			setup: func(db *prevjobsDB) {
				active(db)
				db.tsInvalid["GetPreviewByIdentity"] = true
			},
			rights:  prevjobsAllowRights,
			ignored: "fork waiting for maintainer approval (INV-010)",
		},
		{
			name: "rebuild upsert failure", body: "/rebuild",
			setup: func(db *prevjobsDB) {
				active(db)
				db.bools["GetPreviewByIdentity"] = false
				db.errs["UpsertPreview"] = errors.New("upsert refused")
			},
			rights:  prevjobsAllowRights,
			wantErr: "upsert refused",
		},
		{
			name: "rebuild scaffolding failure", body: "/rebuild",
			mutApp: func(a *store.GetApplicationByIDRow) {
				a.Application.PreviewProtection = store.PreviewProtectionBasicAuth
			},
			setup: func(db *prevjobsDB) {
				active(db)
				db.bools["GetPreviewByIdentity"] = false
				db.errs["CreateGeneratedPreviewEnvVar"] = errors.New("insert refused")
			},
			rights:  prevjobsAllowRights,
			wantErr: "insert refused",
		},
		{
			name: "rebuild promotion failure", body: "/rebuild",
			setup: func(db *prevjobsDB) {
				active(db)
				db.bools["GetPreviewByIdentity"] = false
				db.errs["CreateDeployment"] = errors.New("create refused")
			},
			rights:  prevjobsAllowRights,
			wantErr: "create refused",
		},
		{
			name: "rebuild queued at the capacity cap", body: "/rebuild",
			mutApp: func(a *store.GetApplicationByIDRow) {
				a.Application.PreviewMaxConcurrent = ptr(int32(1))
			},
			setup: func(db *prevjobsDB) {
				active(db)
				db.bools["GetPreviewByIdentity"] = false
			},
			rights:   prevjobsAllowRights,
			accepted: "queued by /rebuild: concurrency cap reached (1 live)",
		},
		{
			name: "rebuild busts the cache", body: "/rebuild",
			setup: func(db *prevjobsDB) {
				active(db)
				db.bools["GetPreviewByIdentity"] = false
			},
			rights:   prevjobsAllowRights,
			accepted: "rebuild (no cache) queued by /rebuild",
		},
		{
			name: "keep on a destroyed preview", body: "/keep",
			setup:   func(db *prevjobsDB) { db.enums["PreviewStatus"] = "destroying" },
			rights:  prevjobsAllowRights,
			ignored: "preview already destroyed — /deploy to bring it back",
		},
		{
			name: "keep persistence failure", body: "/keep",
			setup: func(db *prevjobsDB) {
				active(db)
				db.errs["KeepPreviewAlive"] = errors.New("update refused")
			},
			rights:  prevjobsAllowRights,
			wantErr: "update refused",
		},
		{
			name: "keep resets the TTL", body: "/keep",
			setup: active, rights: prevjobsAllowRights,
			accepted: "TTL reset by /keep",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			q, keyring, logger, db := prevjobsDeps(t)
			if c.setup != nil {
				c.setup(db)
			}
			app := prevjobsApp(c.mutApp)
			event := prevjobsCommentEvent(c.body)
			if c.mutEvent != nil {
				c.mutEvent(&event)
			}
			outcome, err := handlePreviewComment(context.Background(), q, keyring, logger,
				app, store.GitProviderGithub, event, c.rights)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if outcome.Ignored != c.ignored || outcome.Accepted != c.accepted {
				t.Fatalf("outcome = %+v, want ignored=%q accepted=%q", outcome, c.ignored, c.accepted)
			}
		})
	}
}

func TestPrevjobsHandlePreviewCommentRepoFallback(t *testing.T) {
	// The event has no repo reference; the preview's persisted one is used.
	q, keyring, logger, db := prevjobsDeps(t)
	db.enums["PreviewStatus"] = "active"
	db.fillPtr["GetPreviewByIdentity"] = true
	db.strs["GetPreviewByIdentity"] = "persisted/repo"
	event := prevjobsCommentEvent("/keep")
	event.RepoReference = ""
	var seen string
	rights := func(_ context.Context, repo string) (bool, error) {
		seen = repo
		return true, nil
	}
	outcome, err := handlePreviewComment(context.Background(), q, keyring, logger,
		prevjobsApp(nil), store.GitProviderGithub, event, rights)
	if err != nil || outcome.Accepted != "TTL reset by /keep" {
		t.Fatalf("outcome = %+v, err = %v", outcome, err)
	}
	if seen != "persisted/repo" {
		t.Fatalf("rights repo = %q", seen)
	}
}

func TestPrevjobsHandlePreviewCommentWrapper(t *testing.T) {
	t.Run("forge notifier failure propagates", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.GitSourceID = ptr(int64(1)) // default blob: decrypt fails
		})
		if _, err := HandlePreviewComment(context.Background(), q, keyring, logger,
			app, store.GitProviderGitlab, prevjobsCommentEvent("/keep")); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("no git source means no credential", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active"
		outcome, err := HandlePreviewComment(context.Background(), q, keyring, logger,
			prevjobsApp(nil), store.GitProviderGithub, prevjobsCommentEvent("/keep"))
		if err != nil || !strings.HasPrefix(outcome.Ignored, "no_api_credentials") {
			t.Fatalf("outcome = %+v, err = %v", outcome, err)
		}
	})
	t.Run("gitlab token verifies the author end to end", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		srv := prevjobsGitlabServer(t, 40)
		db.enums["PreviewStatus"] = "active"
		db.blobs["GetGitSourceByID"] = prevjobsEncrypt(t, keyring, "git_sources", "api_token_enc", []byte("glpat"))
		db.fillPtr["GetGitSourceByID"] = true
		db.strs["GetGitSourceByID"] = srv.URL
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.GitSourceID = ptr(int64(1))
		})
		event := prevjobsCommentEvent("/keep")
		event.RepoReference = "31"
		outcome, err := HandlePreviewComment(context.Background(), q, keyring, logger,
			app, store.GitProviderGitlab, event)
		if err != nil || outcome.Accepted != "TTL reset by /keep" {
			t.Fatalf("outcome = %+v, err = %v", outcome, err)
		}
	})
}

func TestPrevjobsDeployPreviewForPR(t *testing.T) {
	t.Run("upsert failure", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["UpsertPreview"] = errors.New("upsert refused")
		if _, _, _, err := DeployPreviewForPR(context.Background(), q, keyring, logger,
			prevjobsApp(nil), store.GitProviderGithub, 7, "feature", "cafe1234", false, nil); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("scaffolding failure", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["CreateGeneratedPreviewEnvVar"] = errors.New("insert refused")
		db.bools["UpsertPreview"] = false
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewProtection = store.PreviewProtectionBasicAuth
		})
		if _, _, _, err := DeployPreviewForPR(context.Background(), q, keyring, logger,
			app, store.GitProviderGithub, 7, "feature", "cafe1234", false, nil); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("unapproved fork stays a reservation", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "queued"
		db.tsInvalid["UpsertPreview"] = true
		_, promoted, reason, err := DeployPreviewForPR(context.Background(), q, keyring, logger,
			prevjobsApp(nil), store.GitProviderGithub, 7, "feature", "cafe1234", true, ptr("fork/repo"))
		if err != nil || promoted || reason != "fork waiting for maintainer approval (INV-010)" {
			t.Fatalf("promoted = %v, reason = %q, err = %v", promoted, reason, err)
		}
	})
	t.Run("capacity cap defers the deployment", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "queued"
		db.bools["UpsertPreview"] = false
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.PreviewMaxConcurrent = ptr(int32(1))
		})
		_, promoted, reason, err := DeployPreviewForPR(context.Background(), q, keyring, logger,
			app, store.GitProviderGithub, 7, "feature", "cafe1234", false, nil)
		if err != nil || promoted || !strings.HasPrefix(reason, "concurrency cap reached") {
			t.Fatalf("promoted = %v, reason = %q, err = %v", promoted, reason, err)
		}
	})
	t.Run("promotes the preview", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "queued"
		db.bools["UpsertPreview"] = false
		_, promoted, reason, err := DeployPreviewForPR(context.Background(), q, keyring, logger,
			prevjobsApp(nil), store.GitProviderGithub, 7, "feature", "cafe1234", false, nil)
		if err != nil || !promoted || reason != "" {
			t.Fatalf("promoted = %v, reason = %q, err = %v", promoted, reason, err)
		}
	})
}

// ---------------------------------------------------------------------------
// previewdestroy.go
// ---------------------------------------------------------------------------

// prevjobsDestroyRuntime scripts the typed Docker side of a full teardown:
// one object of each kind so every removal loop runs, and the exec trio the
// Traefik routing-removal verification uses (absence answered immediately).
func prevjobsDestroyRuntime() *dockerfake.Runtime {
	rt := &dockerfake.Runtime{}
	rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
		return []containertypes.Summary{{ID: "c1"}}, nil
	}
	rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
		return volumetypes.ListResponse{Volumes: []*volumetypes.Volume{{Name: "v1"}, nil}}, nil
	}
	rt.NetworkListFn = func(context.Context, networktypes.ListOptions) ([]networktypes.Summary, error) {
		return []networktypes.Summary{{ID: "n1"}}, nil
	}
	rt.ImageListFn = func(context.Context, imagetypes.ListOptions) ([]imagetypes.Summary, error) {
		return []imagetypes.Summary{{ID: "i1"}}, nil
	}
	rt.ImageRemoveFn = func(context.Context, string, imagetypes.RemoveOptions) ([]imagetypes.DeleteResponse, error) {
		return nil, nil
	}
	rt.ContainerExecCreateFn = func(context.Context, string, containertypes.ExecOptions) (containertypes.ExecCreateResponse, error) {
		return containertypes.ExecCreateResponse{ID: "verify"}, nil
	}
	rt.ContainerExecAttachFn = func(context.Context, string, containertypes.ExecAttachOptions) (dockertypes.HijackedResponse, error) {
		client, server := net.Pipe()
		_ = server.Close()
		return dockertypes.HijackedResponse{Conn: client, Reader: bufio.NewReader(client)}, nil
	}
	rt.ContainerExecInspectFn = func(context.Context, string) (containertypes.ExecInspect, error) {
		return containertypes.ExecInspect{ExitCode: 0}, nil
	}
	return rt
}

func TestPrevjobsPreviewDestroyExecute(t *testing.T) {
	job := store.Job{ID: 31, JobType: TypePreviewDestroy, Payload: []byte(`{"preview_id":1}`)}
	handler := func(q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger, rt *dockerfake.Runtime, ops *hostopsfake.Ops, recorder *audit.Recorder) *PreviewDestroy {
		return &PreviewDestroy{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: rt}, HostOps: fixedHost{ops: ops}, Audit: recorder,
		}
	}

	t.Run("invalid payload", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		j := store.Job{ID: 31, JobType: TypePreviewDestroy, Payload: []byte(`{`)}
		h := handler(q, keyring, logger, prevjobsDestroyRuntime(), &hostopsfake.Ops{}, nil)
		if _, err := h.Execute(context.Background(), j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("vanished preview is already gone", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["GetPreviewByID"] = pgx.ErrNoRows
		h := handler(q, keyring, logger, prevjobsDestroyRuntime(), &hostopsfake.Ops{}, nil)
		out, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
		if err != nil || out.(map[string]any)["status"] != "already gone" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("destroyed preview is idempotent", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "destroyed"
		h := handler(q, keyring, logger, prevjobsDestroyRuntime(), &hostopsfake.Ops{}, nil)
		out, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
		if err != nil || out.(map[string]any)["status"] != "already destroyed" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("vanished application gets a tombstone", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active"
		db.errs["GetApplicationByID"] = pgx.ErrNoRows
		h := handler(q, keyring, logger, prevjobsDestroyRuntime(), &hostopsfake.Ops{}, nil)
		out, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
		if err != nil || out.(map[string]any)["status"] != "destroyed (application gone)" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("destination failure propagates", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active"
		db.errs["GetDestinationByID"] = errors.New("destination gone")
		h := handler(q, keyring, logger, prevjobsDestroyRuntime(), &hostopsfake.Ops{}, nil)
		if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("server failure propagates", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active"
		db.errs["GetServerByID"] = errors.New("server gone")
		h := handler(q, keyring, logger, prevjobsDestroyRuntime(), &hostopsfake.Ops{}, nil)
		if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("docker channel unavailable records cleanup_failed", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active"
		h := &PreviewDestroy{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: unavailableDocker{}, HostOps: fixedHost{ops: &hostopsfake.Ops{}},
		}
		if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err == nil ||
			!strings.Contains(err.Error(), "agent channel") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("host channel unavailable records cleanup_failed", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active"
		h := &PreviewDestroy{
			Store: q, Keyring: keyring, Logger: logger,
			Docker: fixedSource{rt: prevjobsDestroyRuntime()}, HostOps: unavailableHost{},
		}
		if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err == nil ||
			!strings.Contains(err.Error(), "agent channel") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("routing removal failure records cleanup_failed", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active" // ProxyType stays traefik
		db.errs["CreateProxyRevision"] = errors.New("revision refused")
		h := handler(q, keyring, logger, prevjobsDestroyRuntime(), &hostopsfake.Ops{}, nil)
		if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err == nil ||
			!strings.Contains(err.Error(), "routing removal") {
			t.Fatalf("err = %v", err)
		}
	})
	sweepFailure := func(name string, breakRT func(rt *dockerfake.Runtime), want string) func(*testing.T) {
		return func(t *testing.T) {
			q, keyring, logger, db := prevjobsDeps(t)
			db.enums["PreviewStatus"] = "active"
			db.enums["ProxyType"] = "none"
			rt := prevjobsDestroyRuntime()
			breakRT(rt)
			h := handler(q, keyring, logger, rt, &hostopsfake.Ops{}, nil)
			if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err == nil ||
				!strings.Contains(err.Error(), want) {
				t.Fatalf("%s: err = %v", name, err)
			}
		}
	}
	t.Run("named container removal failure", sweepFailure("container removal", func(rt *dockerfake.Runtime) {
		rt.ContainerRemoveFn = func(context.Context, string, containertypes.RemoveOptions) error {
			return errors.New("daemon refused")
		}
	}, "container removal"))
	t.Run("container sweep failure", sweepFailure("container sweep", func(rt *dockerfake.Runtime) {
		rt.ContainerListFn = func(context.Context, containertypes.ListOptions) ([]containertypes.Summary, error) {
			return nil, errors.New("daemon refused")
		}
	}, "container sweep"))
	t.Run("volume sweep failure", sweepFailure("volume sweep", func(rt *dockerfake.Runtime) {
		rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
			return volumetypes.ListResponse{}, errors.New("daemon refused")
		}
	}, "volume sweep"))
	t.Run("network sweep failure", sweepFailure("network sweep", func(rt *dockerfake.Runtime) {
		rt.NetworkListFn = func(context.Context, networktypes.ListOptions) ([]networktypes.Summary, error) {
			return nil, errors.New("daemon refused")
		}
	}, "network sweep"))
	t.Run("image sweep failure", sweepFailure("image sweep", func(rt *dockerfake.Runtime) {
		rt.ImageListFn = func(context.Context, imagetypes.ListOptions) ([]imagetypes.Summary, error) {
			return nil, errors.New("daemon refused")
		}
	}, "image sweep"))
	t.Run("preview-label volume sweep failure", sweepFailure("second volume sweep", func(rt *dockerfake.Runtime) {
		calls := 0
		rt.VolumeListFn = func(context.Context, volumetypes.ListOptions) (volumetypes.ListResponse, error) {
			calls++
			if calls > 1 {
				return volumetypes.ListResponse{}, errors.New("daemon refused")
			}
			return volumetypes.ListResponse{}, nil
		}
	}, "volume sweep"))
	t.Run("compose image namespace sweep failure", sweepFailure("second image sweep", func(rt *dockerfake.Runtime) {
		calls := 0
		rt.ImageListFn = func(context.Context, imagetypes.ListOptions) ([]imagetypes.Summary, error) {
			calls++
			if calls > 1 {
				return nil, errors.New("daemon refused")
			}
			return nil, nil
		}
	}, "image sweep"))
	t.Run("instance directory removal failure", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active"
		db.enums["ProxyType"] = "none"
		ops := &hostopsfake.Ops{RemoveFn: func(context.Context, agentwire.FileRemoveParams) error {
			return errors.New("permission denied")
		}}
		h := handler(q, keyring, logger, prevjobsDestroyRuntime(), ops, nil)
		if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err == nil ||
			!strings.Contains(err.Error(), "instance directory removal") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("final status persistence failure propagates", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active"
		db.enums["ProxyType"] = "none"
		db.errs["SetPreviewStatus"] = errors.New("update refused")
		h := handler(q, keyring, logger, prevjobsDestroyRuntime(), &hostopsfake.Ops{}, nil)
		if _, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("full teardown succeeds through routing, sweeps and audit", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.enums["PreviewStatus"] = "active" // ProxyType default: traefik
		db.fillPtr["GetPreviewByID"] = true  // fqdn set: the audit payload carries it
		recorder := &audit.Recorder{Store: q, Logger: logger}
		h := handler(q, keyring, logger, prevjobsDestroyRuntime(), &hostopsfake.Ops{}, recorder)
		out, err := h.Execute(context.Background(), job, queue.NewStepRecorder(q, job))
		if err != nil {
			t.Fatal(err)
		}
		if out.(map[string]any)["destroyed"] != jobFixtureUUID {
			t.Fatalf("out = %#v", out)
		}
	})
}

// ---------------------------------------------------------------------------
// previewfeedback.go
// ---------------------------------------------------------------------------

func TestPrevjobsPreviewCommentBody(t *testing.T) {
	app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
		a.Application.PreviewTtlMinutes = ptr(int32(2880))
	})
	preview := store.Preview{PrID: 7, HeadSha: ptr("cafe1234cafe1234"), Fqdn: ptr("pr-7.apps.example")}
	_ = preview.Uuid.Scan(jobFixtureUUID)

	t.Run("success carries the URL, the dashboard links and the commands", func(t *testing.T) {
		body := previewCommentBody(app, preview, "success", "panel.example")
		for _, want := range []string{
			"https://pr-7.apps.example", "Open in AkerDock", "Build logs",
			"expires after 2d of inactivity", "/rebuild",
		} {
			if !strings.Contains(body, want) {
				t.Fatalf("body missing %q:\n%s", want, body)
			}
		}
	})
	t.Run("success without a domain says so", func(t *testing.T) {
		bare := preview
		bare.Fqdn = nil
		body := previewCommentBody(prevjobsApp(nil), bare, "success", "")
		if !strings.Contains(body, "no domain configured") {
			t.Fatalf("body = %s", body)
		}
	})
	t.Run("awaiting manual deploy explains the switch", func(t *testing.T) {
		body := previewCommentBody(prevjobsApp(nil), preview, "awaiting_manual_deploy", "")
		if !strings.Contains(body, "auto-deploy on open is off") {
			t.Fatalf("body = %s", body)
		}
	})
	t.Run("destroyed and failure and unknown states", func(t *testing.T) {
		if !strings.Contains(previewCommentBody(app, preview, "destroyed", ""), "destroyed") {
			t.Fatal("destroyed body")
		}
		if !strings.Contains(previewCommentBody(app, preview, "failure", ""), "failed") {
			t.Fatal("failure body")
		}
		if !strings.Contains(previewCommentBody(app, preview, "queued", ""), "queued") {
			t.Fatal("queued body")
		}
		if !strings.Contains(previewCommentBody(app, preview, "deploying", ""), "deploying") {
			t.Fatal("deploying body")
		}
		if body := previewCommentBody(app, preview, "something_else", ""); !strings.HasSuffix(strings.TrimSpace(body), "unit-app**") {
			t.Fatalf("unknown state body = %q", body)
		}
	})
}

func TestPrevjobsHumanizeMinutes(t *testing.T) {
	for m, want := range map[int]string{2880: "2d", 120: "2h", 45: "45m"} {
		if got := humanizeMinutes(m); got != want {
			t.Fatalf("humanizeMinutes(%d) = %q, want %q", m, got, want)
		}
	}
}

func TestPrevjobsInstanceFqdn(t *testing.T) {
	t.Run("nil store", func(t *testing.T) {
		f := &PreviewFeedback{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
		if got := f.instanceFqdn(context.Background()); got != "" {
			t.Fatalf("fqdn = %q", got)
		}
	})
	t.Run("configured fqdn", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.fillPtr["GetInstanceSettings"] = true
		db.strs["GetInstanceSettings"] = "panel.example"
		f := &PreviewFeedback{Store: q, Logger: logger}
		if got := f.instanceFqdn(context.Background()); got != "panel.example" {
			t.Fatalf("fqdn = %q", got)
		}
	})
	t.Run("lookup failure", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.errs["GetInstanceSettings"] = errors.New("db gone")
		f := &PreviewFeedback{Store: q, Logger: logger}
		if got := f.instanceFqdn(context.Background()); got != "" {
			t.Fatalf("fqdn = %q", got)
		}
	})
}

func TestPrevjobsNotifyDegradedPaths(t *testing.T) {
	preview := store.Preview{PrID: 7, Provider: store.GitProviderGitlab}
	_ = preview.Uuid.Scan(jobFixtureUUID)

	t.Run("no git source means silence", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		f := &PreviewFeedback{Store: q, Keyring: keyring, Logger: logger}
		f.Notify(context.Background(), prevjobsApp(nil), preview, "success")
	})
	t.Run("unusable credential is logged, never fatal", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.GitSourceID = ptr(int64(1)) // default blob: decrypt fails
		})
		f := &PreviewFeedback{Store: q, Keyring: keyring, Logger: logger}
		f.Notify(context.Background(), app, preview, "success")
	})
	t.Run("notifier without a repo reference warns and stops", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.blobs["GetGitSourceByID"] = prevjobsEncrypt(t, keyring, "git_sources", "api_token_enc", []byte("glpat"))
		app := prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.GitSourceID = ptr(int64(1))
			a.Application.GitRepositoryUrl = ptr("https://git.example/o/r.git")
		})
		f := &PreviewFeedback{Store: q, Keyring: keyring, Logger: logger}
		// preview.RepoReference is nil: notifyForge returns before any HTTP.
		f.Notify(context.Background(), app, preview, "success")
	})
}

// prevjobsForgeNotifier is a recording gitforge.Notifier double.
type prevjobsForgeNotifier struct {
	commentErr error
	statusErr  error
	comments   []string
	statuses   []gitforge.StatusState
}

func (n *prevjobsForgeNotifier) SetCommitStatus(_ context.Context, _, _ string, state gitforge.StatusState, _ string) error {
	n.statuses = append(n.statuses, state)
	return n.statusErr
}

func (n *prevjobsForgeNotifier) UpsertComment(_ context.Context, _ string, _ int, _, body string) error {
	n.comments = append(n.comments, body)
	return n.commentErr
}

func (n *prevjobsForgeNotifier) AuthorCanWrite(context.Context, string, string, int64) (bool, error) {
	return true, nil
}

func TestPrevjobsNotifyForge(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	app := prevjobsApp(nil)
	base := store.Preview{PrID: 7, RepoReference: ptr("31"), HeadSha: ptr("cafe1234"), Fqdn: ptr("pr-7.x")}
	_ = base.Uuid.Scan(jobFixtureUUID)

	t.Run("state mapping", func(t *testing.T) {
		for state, want := range map[string]gitforge.StatusState{
			"queued": gitforge.StatusQueued, "deploying": gitforge.StatusRunning,
			"success": gitforge.StatusSuccess, "failure": gitforge.StatusFailure,
		} {
			n := &prevjobsForgeNotifier{}
			notifyForge(context.Background(), n, logger, app, base, state, "")
			if len(n.comments) != 1 || len(n.statuses) != 1 || n.statuses[0] != want {
				t.Fatalf("state %s: %+v", state, n)
			}
		}
	})
	t.Run("destroyed keeps the last commit status", func(t *testing.T) {
		n := &prevjobsForgeNotifier{}
		notifyForge(context.Background(), n, logger, app, base, "destroyed", "")
		if len(n.comments) != 1 || len(n.statuses) != 0 {
			t.Fatalf("notifier = %+v", n)
		}
	})
	t.Run("unknown state does nothing", func(t *testing.T) {
		n := &prevjobsForgeNotifier{}
		notifyForge(context.Background(), n, logger, app, base, "weird", "")
		if len(n.comments) != 0 {
			t.Fatalf("notifier = %+v", n)
		}
	})
	t.Run("no repo reference stops before any call", func(t *testing.T) {
		n := &prevjobsForgeNotifier{}
		bare := base
		bare.RepoReference = nil
		notifyForge(context.Background(), n, logger, app, bare, "success", "")
		if len(n.comments) != 0 {
			t.Fatalf("notifier = %+v", n)
		}
	})
	t.Run("no head sha skips the commit status", func(t *testing.T) {
		n := &prevjobsForgeNotifier{}
		bare := base
		bare.HeadSha = nil
		notifyForge(context.Background(), n, logger, app, bare, "success", "")
		if len(n.comments) != 1 || len(n.statuses) != 0 {
			t.Fatalf("notifier = %+v", n)
		}
	})
	t.Run("comment and status failures are best-effort", func(t *testing.T) {
		n := &prevjobsForgeNotifier{commentErr: errors.New("403"), statusErr: errors.New("500")}
		notifyForge(context.Background(), n, logger, app, base, "success", "")
		if len(n.comments) != 1 || len(n.statuses) != 1 {
			t.Fatalf("notifier = %+v", n)
		}
	})
}

func TestPrevjobsNotifyGithubApp(t *testing.T) {
	appRow := func() store.GetApplicationByIDRow {
		return prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.GitSourceID = ptr(int64(1))
			a.Application.RepositoryID = ptr(int64(1))
		})
	}
	preview := store.Preview{
		PrID: 7, Provider: store.GitProviderGithub,
		HeadSha: ptr("cafe1234"), Fqdn: ptr("pr-7.x"),
	}
	_ = preview.Uuid.Scan(jobFixtureUUID)

	// wire prepares a feedback whose GitHub App resolves against srvURL.
	wire := func(t *testing.T, srvURL string) (*PreviewFeedback, *prevjobsDB) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.fillPtr["GetGitSourceByID"] = true // github_app_id set: the rich path
		db.fillPtr["GetGithubAppByID"] = true
		db.strs["GetGithubAppByID"] = srvURL
		db.blobs["GetGithubAppByID"] = prevjobsEncrypt(t, keyring,
			"github_apps", "app_private_key_enc", prevjobsRSAKeyPEM(t))
		return &PreviewFeedback{Store: q, Keyring: keyring, Logger: logger}, db
	}

	t.Run("success publishes comment, deployment, status and check", func(t *testing.T) {
		srv := prevjobsGithubServer(t, nil)
		f, _ := wire(t, srv.URL)
		f.Notify(context.Background(), appRow(), preview, "success")
	})
	t.Run("failure maps the deployment and check conclusion", func(t *testing.T) {
		srv := prevjobsGithubServer(t, nil)
		f, _ := wire(t, srv.URL)
		f.Notify(context.Background(), appRow(), preview, "failure")
	})
	t.Run("queued and deploying map the check status", func(t *testing.T) {
		srv := prevjobsGithubServer(t, nil)
		f, _ := wire(t, srv.URL)
		f.Notify(context.Background(), appRow(), preview, "queued")
		f.Notify(context.Background(), appRow(), preview, "deploying")
	})
	t.Run("destroyed never rewrites the last check", func(t *testing.T) {
		srv := prevjobsGithubServer(t, map[string]int{"/check-runs": 500, "/deployments": 500})
		f, _ := wire(t, srv.URL)
		f.Notify(context.Background(), appRow(), preview, "destroyed")
	})
	t.Run("no head sha skips deployment and check", func(t *testing.T) {
		srv := prevjobsGithubServer(t, map[string]int{"/check-runs": 500, "/deployments": 500})
		f, _ := wire(t, srv.URL)
		bare := preview
		bare.HeadSha = nil
		bare.Fqdn = nil
		f.Notify(context.Background(), appRow(), bare, "success")
	})
	t.Run("api failures stay best-effort", func(t *testing.T) {
		srv := prevjobsGithubServer(t, map[string]int{"/comments": 500, "/deployments": 500, "/check-runs": 500})
		f, _ := wire(t, srv.URL)
		f.Notify(context.Background(), appRow(), preview, "success")
	})
	t.Run("deployment status failure is logged", func(t *testing.T) {
		srv := prevjobsGithubServer(t, map[string]int{"/statuses": 500})
		f, _ := wire(t, srv.URL)
		f.Notify(context.Background(), appRow(), preview, "success")
	})
	t.Run("token mint failure is logged", func(t *testing.T) {
		srv := prevjobsGithubServer(t, map[string]int{"/access_tokens": 500})
		f, _ := wire(t, srv.URL)
		f.Notify(context.Background(), appRow(), preview, "success")
	})
	t.Run("undecryptable app key is logged", func(t *testing.T) {
		srv := prevjobsGithubServer(t, nil)
		f, db := wire(t, srv.URL)
		db.blobs["GetGithubAppByID"] = []byte("not ciphertext")
		f.Notify(context.Background(), appRow(), preview, "success")
	})
	t.Run("incomplete app credentials stop silently", func(t *testing.T) {
		srv := prevjobsGithubServer(t, nil)
		f, db := wire(t, srv.URL)
		db.fillPtr["GetGithubAppByID"] = false // app_id and installation_id NULL
		f.Notify(context.Background(), appRow(), preview, "success")
	})
	t.Run("vanished repository stops silently", func(t *testing.T) {
		srv := prevjobsGithubServer(t, nil)
		f, db := wire(t, srv.URL)
		db.errs["GetRepositoryByID"] = pgx.ErrNoRows
		f.Notify(context.Background(), appRow(), preview, "success")
	})
	t.Run("vanished github app stops silently", func(t *testing.T) {
		srv := prevjobsGithubServer(t, nil)
		f, db := wire(t, srv.URL)
		db.errs["GetGithubAppByID"] = pgx.ErrNoRows
		f.Notify(context.Background(), appRow(), preview, "success")
	})
}

// ---------------------------------------------------------------------------
// previewforge.go
// ---------------------------------------------------------------------------

func TestPrevjobsForgeNotifierResolution(t *testing.T) {
	withSource := func(mut func(*store.GetApplicationByIDRow)) store.GetApplicationByIDRow {
		return prevjobsApp(func(a *store.GetApplicationByIDRow) {
			a.Application.GitSourceID = ptr(int64(1))
			if mut != nil {
				mut(a)
			}
		})
	}
	t.Run("no git source", func(t *testing.T) {
		q, keyring, _, _ := prevjobsDeps(t)
		n, err := forgeNotifier(context.Background(), q, keyring, prevjobsApp(nil), store.GitProviderGitlab)
		if n != nil || err != nil {
			t.Fatalf("n = %v, err = %v", n, err)
		}
	})
	t.Run("git source lookup failure", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.errs["GetGitSourceByID"] = errors.New("db gone")
		if _, err := forgeNotifier(context.Background(), q, keyring, withSource(nil), store.GitProviderGitlab); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("no api token", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.blobs["GetGitSourceByID"] = nil // explicit SQL NULL
		n, err := forgeNotifier(context.Background(), q, keyring, withSource(nil), store.GitProviderGitlab)
		if n != nil || err != nil {
			t.Fatalf("n = %v, err = %v", n, err)
		}
	})
	t.Run("undecryptable token", func(t *testing.T) {
		q, keyring, _, _ := prevjobsDeps(t)
		if _, err := forgeNotifier(context.Background(), q, keyring, withSource(nil), store.GitProviderGitlab); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("pinned api url builds a gitlab client", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.blobs["GetGitSourceByID"] = prevjobsEncrypt(t, keyring, "git_sources", "api_token_enc", []byte("glpat"))
		db.fillPtr["GetGitSourceByID"] = true
		db.strs["GetGitSourceByID"] = "https://gl.example/api/v4/"
		n, err := forgeNotifier(context.Background(), q, keyring, withSource(nil), store.GitProviderGitlab)
		if err != nil {
			t.Fatal(err)
		}
		gl, ok := n.(*gitforge.GitLab)
		if !ok || gl.BaseURL != "https://gl.example/api/v4" || gl.Token != "glpat" {
			t.Fatalf("notifier = %#v", n)
		}
	})
	t.Run("derived api url builds a gitea client", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.blobs["GetGitSourceByID"] = prevjobsEncrypt(t, keyring, "git_sources", "api_token_enc", []byte("tkn"))
		app := withSource(func(a *store.GetApplicationByIDRow) {
			a.Application.GitRepositoryUrl = ptr("git@gitea.example:org/repo.git")
		})
		n, err := forgeNotifier(context.Background(), q, keyring, app, store.GitProviderGitea)
		if err != nil {
			t.Fatal(err)
		}
		gt, ok := n.(*gitforge.Gitea)
		if !ok || gt.BaseURL != "https://gitea.example/api/v1" {
			t.Fatalf("notifier = %#v", n)
		}
	})
	t.Run("no derivable url", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.blobs["GetGitSourceByID"] = prevjobsEncrypt(t, keyring, "git_sources", "api_token_enc", []byte("tkn"))
		n, err := forgeNotifier(context.Background(), q, keyring, withSource(nil), store.GitProviderGitlab)
		if n != nil || err != nil {
			t.Fatalf("n = %v, err = %v", n, err)
		}
	})
	t.Run("github stays on the rich path", func(t *testing.T) {
		q, keyring, _, db := prevjobsDeps(t)
		db.blobs["GetGitSourceByID"] = prevjobsEncrypt(t, keyring, "git_sources", "api_token_enc", []byte("tkn"))
		db.fillPtr["GetGitSourceByID"] = true
		db.strs["GetGitSourceByID"] = "https://api.github.example"
		n, err := forgeNotifier(context.Background(), q, keyring, withSource(nil), store.GitProviderGithub)
		if n != nil || err != nil {
			t.Fatalf("n = %v, err = %v", n, err)
		}
	})
}

func TestPrevjobsDefaultForgeAPIURL(t *testing.T) {
	cases := []struct {
		name     string
		provider store.GitProvider
		repo     *string
		want     string
	}{
		{"nil url", store.GitProviderGitlab, nil, ""},
		{"empty url", store.GitProviderGitlab, ptr(""), ""},
		{"unparsable url", store.GitProviderGitlab, ptr("just-a-name"), ""},
		{"gitlab https", store.GitProviderGitlab, ptr("https://gl.example/o/r.git"), "https://gl.example/api/v4"},
		{"gitea scp-like", store.GitProviderGitea, ptr("git@gitea.example:o/r.git"), "https://gitea.example/api/v1"},
		{"github has no default", store.GitProviderGithub, ptr("https://github.example/o/r.git"), ""},
		{"broken scheme url", store.GitProviderGitlab, ptr("https://%zz/o/r"), ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := defaultForgeAPIURL(c.provider, c.repo); got != c.want {
				t.Fatalf("defaultForgeAPIURL = %q, want %q", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// githubapppush.go
// ---------------------------------------------------------------------------

func TestPrevjobsGithubAppPushExecute(t *testing.T) {
	run := func(t *testing.T, q *store.Queries, logger *slog.Logger) (map[string]any, error) {
		t.Helper()
		j := store.Job{
			ID: 41, JobType: TypeGithubAppPush,
			Payload: []byte(`{"delivery_id":1,"github_app_id":1,"repository_external_id":"42"}`),
		}
		out, err := (&GithubAppPush{Store: q, Logger: logger}).
			Execute(context.Background(), j, queue.NewStepRecorder(q, j))
		if out == nil {
			return nil, err
		}
		return out.(map[string]any), err
	}

	t.Run("invalid payload", func(t *testing.T) {
		q, _, logger, _ := prevjobsDeps(t)
		j := store.Job{ID: 41, JobType: TypeGithubAppPush, Payload: []byte(`{`)}
		if _, err := (&GithubAppPush{Store: q, Logger: logger}).
			Execute(context.Background(), j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("missing delivery", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.errs["GetWebhookDeliveryByID"] = pgx.ErrNoRows
		if _, err := run(t, q, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("invalid signature refused", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.bools["GetWebhookDeliveryByID"] = false
		if _, err := run(t, q, logger); err == nil || !strings.Contains(err.Error(), "invalid signature") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unparsable push fails the delivery", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.blobs["GetWebhookDeliveryByID"] = []byte("not json")
		if _, err := run(t, q, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("branch deletion deploys nothing", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.blobs["GetWebhookDeliveryByID"] = prevjobsPushJSON("refs/heads/main", "0000000000")
		out, err := run(t, q, logger)
		if err != nil || out["reason"] != "branch deleted" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("application list failure propagates", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.blobs["GetWebhookDeliveryByID"] = prevjobsPushJSON("refs/heads/main", "cafe1234")
		db.errs["ListApplicationIDsForRepositoryPush"] = errors.New("db gone")
		if _, err := run(t, q, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("no bound application", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.blobs["GetWebhookDeliveryByID"] = prevjobsPushJSON("refs/heads/main", "cafe1234")
		db.rows["ListApplicationIDsForRepositoryPush"] = 0
		out, err := run(t, q, logger)
		if err != nil || out["reason"] != "no application bound" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("vanished application counts as no acceptance", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.blobs["GetWebhookDeliveryByID"] = prevjobsPushJSON("refs/heads/main", "cafe1234")
		db.errs["GetApplicationByID"] = pgx.ErrNoRows
		out, err := run(t, q, logger)
		if err != nil || out["status"] != "ignored" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("policy refusal is recorded per application", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.blobs["GetWebhookDeliveryByID"] = prevjobsPushJSON("refs/heads/dev", "cafe1234")
		out, err := run(t, q, logger)
		if err != nil {
			t.Fatal(err)
		}
		got := out["applications"].(map[string]string)[jobFixtureUUID]
		if !strings.HasPrefix(got, "ignored: push on branch dev") {
			t.Fatalf("applications = %#v", out["applications"])
		}
	})
	t.Run("full queue records the failure per application", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.blobs["GetWebhookDeliveryByID"] = prevjobsPushJSON("refs/heads/main", "cafe1234")
		// active (1, the default) >= server queue limit (1, the default)
		out, err := run(t, q, logger)
		if err != nil {
			t.Fatal(err)
		}
		got := out["applications"].(map[string]string)[jobFixtureUUID]
		if !strings.HasPrefix(got, "failed: ") {
			t.Fatalf("applications = %#v", out["applications"])
		}
	})
	t.Run("accepted push queues a deployment", func(t *testing.T) {
		q, _, logger, db := prevjobsDeps(t)
		db.blobs["GetWebhookDeliveryByID"] = prevjobsPushJSON("refs/heads/main", "cafe1234")
		db.ints["CountActiveDeploymentsForServer"] = 0
		out, err := run(t, q, logger)
		if err != nil || out["status"] != "accepted" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
		got := out["applications"].(map[string]string)[jobFixtureUUID]
		if !strings.HasPrefix(got, "deployment ") {
			t.Fatalf("applications = %#v", out["applications"])
		}
	})
}

// ---------------------------------------------------------------------------
// webhookprocess.go — Execute
// ---------------------------------------------------------------------------

func prevjobsWebhookJob() store.Job {
	return store.Job{ID: 51, JobType: TypeWebhookProcess, Payload: []byte(`{"delivery_id":1}`)}
}

// prevjobsDelivery primes the delivery row: application bound, the given
// event type, the given raw payload.
func prevjobsDelivery(db *prevjobsDB, eventType string, payload []byte) {
	db.fillPtr["GetWebhookDeliveryByID"] = true
	db.strs["GetWebhookDeliveryByID"] = eventType
	db.blobs["GetWebhookDeliveryByID"] = payload
}

func TestPrevjobsWebhookProcessExecutePush(t *testing.T) {
	run := func(t *testing.T, q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger) (map[string]any, error) {
		t.Helper()
		j := prevjobsWebhookJob()
		out, err := (&WebhookProcess{Store: q, Keyring: keyring, Logger: logger}).
			Execute(context.Background(), j, queue.NewStepRecorder(q, j))
		if out == nil {
			return nil, err
		}
		return out.(map[string]any), err
	}

	t.Run("invalid payload", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		j := store.Job{ID: 51, JobType: TypeWebhookProcess, Payload: []byte(`{`)}
		if _, err := (&WebhookProcess{Store: q, Keyring: keyring, Logger: logger}).
			Execute(context.Background(), j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("missing delivery", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["GetWebhookDeliveryByID"] = pgx.ErrNoRows
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("invalid signature refused", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.bools["GetWebhookDeliveryByID"] = false
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("endpoint without an application", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		out, err := run(t, q, keyring, logger) // application_id stays NULL
		if err != nil || out["reason"] != "no application associated with this endpoint" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("non-push event ignored", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "ping", []byte(`{}`))
		out, err := run(t, q, keyring, logger)
		if err != nil || out["reason"] != "event ping is not a push" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("unparsable push fails the delivery", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "push", []byte("not json"))
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("branch deletion ignored", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "push", prevjobsPushJSON("refs/heads/main", "000000"))
		out, err := run(t, q, keyring, logger)
		if err != nil || out["reason"] != "branch deleted" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("vanished application", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "push", prevjobsPushJSON("refs/heads/main", "cafe1234"))
		db.errs["GetApplicationByID"] = pgx.ErrNoRows
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("policy refusal ignored with its reason", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "push", prevjobsPushJSON("refs/heads/dev", "cafe1234"))
		db.fillPtr["GetApplicationByID"] = true
		db.strs["GetApplicationByID"] = "main" // git_branch pinned to main
		out, err := run(t, q, keyring, logger)
		if err != nil || !strings.Contains(out["reason"].(string), "push on branch dev") {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("deployment failure fails the delivery", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "push", prevjobsPushJSON("refs/heads/main", "cafe1234"))
		// active (1) >= queue limit (1): the enqueue refuses
		if _, err := run(t, q, keyring, logger); err == nil ||
			!strings.Contains(err.Error(), "queue is full") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("accepted push deploys", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "push", prevjobsPushJSON("refs/heads/main", "cafe1234"))
		db.ints["CountActiveDeploymentsForServer"] = 0
		out, err := run(t, q, keyring, logger)
		if err != nil || out["status"] != "accepted" || out["commit"] != "cafe1234" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
}

func TestPrevjobsWebhookProcessExecutePullRequest(t *testing.T) {
	run := func(t *testing.T, q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger) (map[string]any, error) {
		t.Helper()
		j := prevjobsWebhookJob()
		out, err := (&WebhookProcess{Store: q, Keyring: keyring, Logger: logger}).
			Execute(context.Background(), j, queue.NewStepRecorder(q, j))
		if out == nil {
			return nil, err
		}
		return out.(map[string]any), err
	}

	t.Run("previews disabled ignored", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "pull_request", prevjobsPRJSON("opened", 42, false))
		db.bools["GetApplicationByID"] = false
		out, err := run(t, q, keyring, logger)
		if err != nil || out["reason"] != "previews disabled" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("unparsable payload fails the delivery", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "pull_request", []byte("not json"))
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("lifecycle failure fails the delivery", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "pull_request", prevjobsPRJSON("opened", 42, false))
		db.errs["UpsertPreview"] = errors.New("upsert refused")
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("closed PR accepted", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "pull_request", prevjobsPRJSON("closed", 42, false))
		db.enums["PreviewStatus"] = "destroyed"
		out, err := run(t, q, keyring, logger)
		if err != nil || out["outcome"] != "already destroyed" || out["action"] != "closed" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("vanished application", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "pull_request", prevjobsPRJSON("opened", 42, false))
		db.errs["GetApplicationByID"] = pgx.ErrNoRows
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
}

func TestPrevjobsWebhookProcessExecuteComment(t *testing.T) {
	run := func(t *testing.T, q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger) (map[string]any, error) {
		t.Helper()
		j := prevjobsWebhookJob()
		out, err := (&WebhookProcess{Store: q, Keyring: keyring, Logger: logger}).
			Execute(context.Background(), j, queue.NewStepRecorder(q, j))
		if out == nil {
			return nil, err
		}
		return out.(map[string]any), err
	}

	t.Run("unparsable comment ignored", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "issue_comment", []byte("not json"))
		out, err := run(t, q, keyring, logger)
		if err != nil || out["reason"] != "unparsable comment payload" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("vanished application", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "issue_comment", prevjobsCommentJSON("/keep"))
		db.errs["GetApplicationByID"] = pgx.ErrNoRows
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("credential failure fails the delivery", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "issue_comment", prevjobsCommentJSON("/keep"))
		db.fillPtr["GetApplicationByID"] = true // git_source_id set, token undecryptable
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("refused command is recorded as ignored", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prevjobsDelivery(db, "issue_comment", prevjobsCommentJSON("/keep"))
		db.enums["PreviewStatus"] = "active"
		out, err := run(t, q, keyring, logger) // no git source: no credential
		if err != nil || !strings.HasPrefix(out["reason"].(string), "no_api_credentials") {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("gitlab note command accepted end to end", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		srv := prevjobsGitlabServer(t, 40)
		prevjobsDelivery(db, "Note Hook", prevjobsGitlabNoteJSON)
		db.enums["WebhookProvider"] = "gitlab"
		db.enums["PreviewStatus"] = "active"
		db.fillPtr["GetApplicationByID"] = true // git_source_id set
		db.blobs["GetGitSourceByID"] = prevjobsEncrypt(t, keyring, "git_sources", "api_token_enc", []byte("glpat"))
		db.fillPtr["GetGitSourceByID"] = true
		db.strs["GetGitSourceByID"] = srv.URL
		out, err := run(t, q, keyring, logger)
		if err != nil || out["status"] != "accepted" || out["outcome"] != "TTL reset by /keep" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
}

func TestPrevjobsEnqueueWebhookDeployment(t *testing.T) {
	app := prevjobsApp(nil)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	fail := func(name string, prime func(db *prevjobsDB)) func(*testing.T) {
		return func(t *testing.T) {
			q, _, _, db := prevjobsDeps(t)
			db.ints["CountActiveDeploymentsForServer"] = 0
			if prime != nil {
				prime(db)
			}
			db.errs[name] = errors.New(name + " failed")
			if _, err := EnqueueWebhookDeployment(context.Background(), q, logger, app); err == nil {
				t.Fatalf("want %s error", name)
			}
		}
	}
	t.Run("destination failure", fail("GetDestinationByID", nil))
	t.Run("server failure", fail("GetServerByID", nil))
	t.Run("count failure", fail("CountActiveDeploymentsForServer", nil))
	t.Run("create failure", fail("CreateDeployment", nil))
	t.Run("supersede failure", fail("SupersedeQueuedDeployments", nil))
	t.Run("cancel failure", fail("CancelJobsForDeployments", nil))
	t.Run("enqueue failure", fail("EnqueueJob", nil))
	t.Run("full queue refused", func(t *testing.T) {
		q, _, _, _ := prevjobsDeps(t) // active 1 >= limit 1
		if _, err := EnqueueWebhookDeployment(context.Background(), q, logger, app); err == nil ||
			!strings.Contains(err.Error(), "queue is full") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("success without coalescing", func(t *testing.T) {
		q, _, _, db := prevjobsDeps(t)
		db.ints["CountActiveDeploymentsForServer"] = 0
		db.rows["SupersedeQueuedDeployments"] = 0
		if _, err := EnqueueWebhookDeployment(context.Background(), q, logger, app); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("success coalescing an older push", func(t *testing.T) {
		q, _, _, db := prevjobsDeps(t)
		db.ints["CountActiveDeploymentsForServer"] = 0
		deployment, err := EnqueueWebhookDeployment(context.Background(), q, logger, app)
		if err != nil || deployment.ID == 0 {
			t.Fatalf("deployment = %+v, err = %v", deployment, err)
		}
	})
}

// ---------------------------------------------------------------------------
// githubappcomment.go
// ---------------------------------------------------------------------------

func TestPrevjobsGithubAppIssueCommentExecute(t *testing.T) {
	run := func(t *testing.T, q *store.Queries, keyring *envelope.Keyring, logger *slog.Logger) (map[string]any, error) {
		t.Helper()
		j := store.Job{
			ID: 61, JobType: TypeGithubAppIssueComment,
			Payload: []byte(`{"delivery_id":1,"github_app_id":1}`),
		}
		out, err := (&GithubAppIssueComment{Store: q, Keyring: keyring, Logger: logger}).
			Execute(context.Background(), j, queue.NewStepRecorder(q, j))
		if out == nil {
			return nil, err
		}
		return out.(map[string]any), err
	}
	prime := func(db *prevjobsDB, payload []byte) {
		db.blobs["GetWebhookDeliveryByID"] = payload
		db.enums["PreviewStatus"] = "active"
	}

	t.Run("invalid payload", func(t *testing.T) {
		q, keyring, logger, _ := prevjobsDeps(t)
		j := store.Job{ID: 61, JobType: TypeGithubAppIssueComment, Payload: []byte(`{`)}
		if _, err := (&GithubAppIssueComment{Store: q, Keyring: keyring, Logger: logger}).
			Execute(context.Background(), j, queue.NewStepRecorder(q, j)); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("missing delivery", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.errs["GetWebhookDeliveryByID"] = pgx.ErrNoRows
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("unverified delivery refused", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		db.bools["GetWebhookDeliveryByID"] = false
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("unparsable comment payload", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, []byte("not json"))
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("plain comment carries no command", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsCommentJSON("nice work"))
		out, err := run(t, q, keyring, logger)
		if err != nil || out["status"] != "ignored" {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("application list failure propagates", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsCommentJSON("/keep"))
		db.errs["ListApplicationIDsForRepositoryPush"] = errors.New("db gone")
		if _, err := run(t, q, keyring, logger); err == nil {
			t.Fatal("want error")
		}
	})
	t.Run("no bound application finishes ignored", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsCommentJSON("/keep"))
		db.rows["ListApplicationIDsForRepositoryPush"] = 0
		out, err := run(t, q, keyring, logger)
		if err != nil || len(out["applications"].(map[string]string)) != 0 {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("vanished application is skipped", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsCommentJSON("/keep"))
		db.errs["GetApplicationByID"] = pgx.ErrNoRows
		out, err := run(t, q, keyring, logger)
		if err != nil || len(out["applications"].(map[string]string)) != 0 {
			t.Fatalf("out = %#v, err = %v", out, err)
		}
	})
	t.Run("incomplete app credentials refuse the command", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsCommentJSON("/keep")) // AppID and co stay NULL
		out, err := run(t, q, keyring, logger)
		if err != nil {
			t.Fatal(err)
		}
		got := out["applications"].(map[string]string)[jobFixtureUUID]
		if !strings.HasPrefix(got, "ignored: no_api_credentials") {
			t.Fatalf("applications = %#v", out["applications"])
		}
	})
	t.Run("github app lookup failure degrades to no credential", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsCommentJSON("/keep"))
		db.errs["GetGithubAppByID"] = errors.New("db gone")
		out, err := run(t, q, keyring, logger)
		if err != nil {
			t.Fatal(err)
		}
		got := out["applications"].(map[string]string)[jobFixtureUUID]
		if !strings.HasPrefix(got, "ignored: no_api_credentials") {
			t.Fatalf("applications = %#v", out["applications"])
		}
	})
	t.Run("undecryptable app key degrades to no credential", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		prime(db, prevjobsCommentJSON("/keep"))
		db.fillPtr["GetGithubAppByID"] = true // key blob stays undecryptable
		out, err := run(t, q, keyring, logger)
		if err != nil {
			t.Fatal(err)
		}
		got := out["applications"].(map[string]string)[jobFixtureUUID]
		if !strings.HasPrefix(got, "ignored: no_api_credentials") {
			t.Fatalf("applications = %#v", out["applications"])
		}
	})
	t.Run("token mint failure degrades to no credential", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		srv := prevjobsGithubServer(t, map[string]int{"/access_tokens": 500})
		prime(db, prevjobsCommentJSON("/keep"))
		db.fillPtr["GetGithubAppByID"] = true
		db.strs["GetGithubAppByID"] = srv.URL
		db.blobs["GetGithubAppByID"] = prevjobsEncrypt(t, keyring,
			"github_apps", "app_private_key_enc", prevjobsRSAKeyPEM(t))
		out, err := run(t, q, keyring, logger)
		if err != nil {
			t.Fatal(err)
		}
		got := out["applications"].(map[string]string)[jobFixtureUUID]
		if !strings.HasPrefix(got, "ignored: no_api_credentials") {
			t.Fatalf("applications = %#v", out["applications"])
		}
	})
	t.Run("verified author executes the command", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		srv := prevjobsGithubServer(t, nil)
		prime(db, prevjobsCommentJSON("/keep"))
		db.fillPtr["GetGithubAppByID"] = true
		db.strs["GetGithubAppByID"] = srv.URL
		db.blobs["GetGithubAppByID"] = prevjobsEncrypt(t, keyring,
			"github_apps", "app_private_key_enc", prevjobsRSAKeyPEM(t))
		out, err := run(t, q, keyring, logger)
		if err != nil {
			t.Fatal(err)
		}
		got := out["applications"].(map[string]string)[jobFixtureUUID]
		if got != "TTL reset by /keep" {
			t.Fatalf("applications = %#v", out["applications"])
		}
		if out["command"] != "keep" {
			t.Fatalf("out = %#v", out)
		}
	})
	t.Run("command failure is recorded per application", func(t *testing.T) {
		q, keyring, logger, db := prevjobsDeps(t)
		srv := prevjobsGithubServer(t, nil)
		prime(db, prevjobsCommentJSON("/keep"))
		db.fillPtr["GetGithubAppByID"] = true
		db.strs["GetGithubAppByID"] = srv.URL
		db.blobs["GetGithubAppByID"] = prevjobsEncrypt(t, keyring,
			"github_apps", "app_private_key_enc", prevjobsRSAKeyPEM(t))
		db.errs["KeepPreviewAlive"] = errors.New("update refused")
		out, err := run(t, q, keyring, logger)
		if err != nil {
			t.Fatal(err)
		}
		got := out["applications"].(map[string]string)[jobFixtureUUID]
		if !strings.HasPrefix(got, "failed: ") {
			t.Fatalf("applications = %#v", out["applications"])
		}
	})
}
