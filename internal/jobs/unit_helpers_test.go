package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/compose"
	"github.com/deepteams/akerdock/internal/gitforge"
	"github.com/deepteams/akerdock/internal/gitwebhook"
	"github.com/deepteams/akerdock/internal/queue"
	"github.com/deepteams/akerdock/internal/sshexec"
	"github.com/deepteams/akerdock/internal/store"
)

type executable func(context.Context, store.Job, *queue.StepRecorder) (any, error)

func TestJobExecutorsRejectMalformedPayloadBeforeDependencies(t *testing.T) {
	tests := map[string]executable{
		"proxy lifecycle":       (&ProxyLifecycle{}).Execute,
		"server cleanup":        (&ServerCleanup{}).Execute,
		"application lifecycle": (&ApplicationLifecycle{}).Execute,
		"database":              (&DatabaseRun{}).Execute,
		"server validation":     (&ServerValidate{}).Execute,
		"github pull request":   (&GithubAppPullRequest{}).Execute,
		"webhook":               (&WebhookProcess{}).Execute,
		"scheduled task":        (&ScheduledTaskRun{}).Execute,
		"adoption scan":         (&Adoption{}).ExecuteScan,
		"adoption apply":        (&Adoption{}).ExecuteAdopt,
		"adoption disown":       (&Adoption{}).ExecuteDisown,
		"routing":               (&ApplyRouting{}).Execute,
		"deployment":            (&DeploymentRun{}).Execute,
		"certificate":           (&CertificateSync{}).Execute,
		"github comment":        (&GithubAppIssueComment{}).Execute,
		"preview destroy":       (&PreviewDestroy{}).Execute,
		"github push":           (&GithubAppPush{}).Execute,
		"backup":                (&BackupRun{}).Execute,
		"application delete":    (&ApplicationDelete{}).Execute,
	}
	for name, execute := range tests {
		t.Run(name, func(t *testing.T) {
			result, err := execute(context.Background(), store.Job{Payload: []byte("{")}, nil)
			if err == nil || result != nil || !strings.Contains(err.Error(), "invalid payload") {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
		})
	}
}

func TestPinnedHostKey(t *testing.T) {
	if got := PinnedHostKey(store.Server{}); got != "" {
		t.Fatalf("first contact key = %q", got)
	}
	key := "SHA256:known"
	if got := pinnedHostKey(store.Server{HostKeyFingerprint: &key}); got != key {
		t.Fatalf("pinned key = %q", got)
	}
}

func TestStackLifecycleCommands(t *testing.T) {
	stop := stackLifecycleCommand("stop", "--filter label=x", 17)
	if !strings.Contains(stop, "docker stop -t 17") || !strings.Contains(stop, "--filter label=x") {
		t.Fatalf("stop command = %q", stop)
	}
	start := stackLifecycleCommand("start", "labels", 1)
	if !strings.Contains(start, "akerdock.oneshot=true") || !strings.Contains(start, "docker start") {
		t.Fatalf("start command = %q", start)
	}
	restart := stackLifecycleCommand("restart", "labels", 9)
	if !strings.Contains(restart, "docker restart -t 9") {
		t.Fatalf("restart command = %q", restart)
	}
	if got := stackLifecycleCommand("invalid", "labels", 1); got != "" {
		t.Fatalf("invalid command = %q", got)
	}
}

func TestDatabaseProxyDecision(t *testing.T) {
	mode := store.PublicAccessModeTcpProxy
	if tcpProxied(store.Database{}) {
		t.Fatal("private database reported as proxied")
	}
	if tcpProxied(store.Database{IsPublic: true}) {
		t.Fatal("public database without mode reported as proxied")
	}
	if !tcpProxied(store.Database{IsPublic: true, PublicAccessMode: &mode}) {
		t.Fatal("tcp-proxy database not detected")
	}
	direct := store.PublicAccessModePortMapping
	if tcpProxied(store.Database{IsPublic: true, PublicAccessMode: &direct}) {
		t.Fatal("direct database reported as proxied")
	}
}

func TestDeploymentNamingAndPreviewRefs(t *testing.T) {
	appUUID := mustUUID(t, "11111111-1111-4111-8111-111111111111")
	previewUUID := mustUUID(t, "22222222-2222-4222-8222-222222222222")
	run := &deploymentRun{app: store.GetApplicationByIDRow{Resource: store.Resource{Uuid: appUUID}}}
	if got := run.namingIdentity(); got != "11111111-1111-4111-8111-111111111111" {
		t.Fatalf("application identity = %q", got)
	}
	if run.previewFetchRef() != "" {
		t.Fatal("production run returned a preview ref")
	}

	tests := map[store.GitProvider]string{
		store.GitProviderGithub: "refs/pull/42/head",
		store.GitProviderGitea:  "refs/pull/42/head",
		store.GitProviderGitlab: "refs/merge-requests/42/head",
		store.GitProviderOther:  "",
	}
	for provider, want := range tests {
		run.preview = &store.Preview{Uuid: previewUUID, Provider: provider, PrID: 42}
		if got := run.previewFetchRef(); got != want {
			t.Errorf("%s ref = %q, want %q", provider, got, want)
		}
		if got := run.namingIdentity(); got != "22222222-2222-4222-8222-222222222222" {
			t.Errorf("%s preview identity = %q", provider, got)
		}
	}
}

func TestSmallDeploymentHelpers(t *testing.T) {
	if ptrStr("x") == nil || *ptrStr("x") != "x" {
		t.Fatal("ptrStr failed")
	}
	for _, n := range []int{-1, 0, 1} {
		if got := randIndex(n); got != 0 {
			t.Fatalf("randIndex(%d) = %d", n, got)
		}
	}
	for i := 0; i < 50; i++ {
		if got := randIndex(7); got < 0 || got >= 7 {
			t.Fatalf("randIndex(7) = %d", got)
		}
	}
}

func TestForgeURLHelpers(t *testing.T) {
	httpsRepo := "https://gitlab.example.test/acme/app.git"
	scpRepo := "git@gitea.example.test:acme/app.git"
	if got := defaultForgeAPIURL(store.GitProviderGitlab, &httpsRepo); got != "https://gitlab.example.test/api/v4" {
		t.Fatalf("GitLab API URL = %q", got)
	}
	if got := defaultForgeAPIURL(store.GitProviderGitea, &scpRepo); got != "https://gitea.example.test/api/v1" {
		t.Fatalf("Gitea API URL = %q", got)
	}
	if got := defaultForgeAPIURL(store.GitProviderGithub, &httpsRepo); got != "" {
		t.Fatalf("GitHub fallback = %q", got)
	}
	if got := defaultForgeAPIURL(store.GitProviderGitlab, nil); got != "" {
		t.Fatalf("nil repository fallback = %q", got)
	}
	for raw, want := range map[string]string{
		"ssh://git@example.test/acme/app": "example.test",
		"git@example.test:acme/app":       "example.test",
		"relative/repo":                   "",
		"://broken":                       "",
	} {
		if got := repoHost(raw); got != want {
			t.Errorf("repoHost(%q) = %q, want %q", raw, got, want)
		}
	}
	fqdn := "pr-42.example.test"
	if got := previewStatusURL(store.Preview{Fqdn: &fqdn}); got != "https://"+fqdn {
		t.Fatalf("preview URL = %q", got)
	}
	if got := previewStatusURL(store.Preview{}); got != "" {
		t.Fatalf("empty preview URL = %q", got)
	}
}

func TestShortSHA(t *testing.T) {
	short := "abc"
	long := "0123456789abcdef"
	empty := ""
	for name, tc := range map[string]struct {
		in   *string
		want string
	}{
		"nil":   {nil, "?"},
		"empty": {&empty, "?"},
		"short": {&short, short},
		"long":  {&long, "0123456789ab"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := shortSHA(tc.in); got != tc.want {
				t.Fatalf("shortSHA = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPreviewCommentPolicyStopsBeforePersistence(t *testing.T) {
	app := store.GetApplicationByIDRow{Application: store.Application{
		PreviewCommentCommandsEnabled: true,
		PreviewsEnabled:               true,
	}}
	tests := []struct {
		name  string
		app   store.GetApplicationByIDRow
		event gitwebhook.CommentEvent
		want  string
	}{
		{
			name: "plain issue", app: app,
			event: gitwebhook.CommentEvent{Body: "/deploy"},
			want:  "not a comment on a pull request",
		},
		{
			name: "no command", app: app,
			event: gitwebhook.CommentEvent{OnPullRequest: true, Body: "please deploy"},
			want:  "no command in the first line",
		},
		{
			name:  "commands disabled",
			app:   store.GetApplicationByIDRow{Application: store.Application{PreviewsEnabled: true}},
			event: gitwebhook.CommentEvent{OnPullRequest: true, Body: "/deploy"},
			want:  "comment commands disabled",
		},
		{
			name: "previews disabled",
			app: store.GetApplicationByIDRow{Application: store.Application{
				PreviewCommentCommandsEnabled: true,
			}},
			event: gitwebhook.CommentEvent{OnPullRequest: true, Body: "/destroy"},
			want:  "previews disabled",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out, err := handlePreviewComment(
				context.Background(), nil, nil, slog.Default(), tc.app,
				store.GitProviderGithub, tc.event, nil,
			)
			if err != nil || !strings.Contains(out.Ignored, tc.want) {
				t.Fatalf("outcome = %#v, error = %v", out, err)
			}
		})
	}
}

type previewNotifierFake struct {
	comments int
	statuses int
	rights   bool
}

func (f *previewNotifierFake) SetCommitStatus(context.Context, string, string, gitforge.StatusState, string) error {
	f.statuses++
	return nil
}
func (f *previewNotifierFake) UpsertComment(context.Context, string, int, string, string) error {
	f.comments++
	return nil
}
func (f *previewNotifierFake) AuthorCanWrite(context.Context, string, string, int64) (bool, error) {
	return f.rights, nil
}
func (f *previewNotifierFake) CollaboratorCanWrite(context.Context, string, string, string) (bool, error) {
	return f.rights, nil
}

func TestPreviewFeedbackStatesAndRightsAdapter(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	repo := "deepteams/akerdock"
	sha := "0123456789abcdef"
	fqdn := "pr-42.example.test"
	app := store.GetApplicationByIDRow{Resource: store.Resource{
		Uuid: mustUUID(t, "11111111-1111-4111-8111-111111111111"), Name: "unit",
	}}
	preview := store.Preview{
		Uuid: mustUUID(t, "22222222-2222-4222-8222-222222222222"),
		PrID: 42, RepoReference: &repo, HeadSha: &sha, Fqdn: &fqdn,
	}
	notifier := &previewNotifierFake{rights: true}
	for _, state := range []string{"queued", "deploying", "success", "failure", "destroyed", "unknown"} {
		notifyForge(context.Background(), notifier, logger, app, preview, state, "akerdock.example.com")
	}
	if notifier.comments != 5 || notifier.statuses != 4 {
		t.Fatalf("comments = %d, statuses = %d", notifier.comments, notifier.statuses)
	}

	rights := githubAppRights(notifier, "token", "unit")
	if ok, err := rights(context.Background(), repo); err != nil || !ok {
		t.Fatalf("rights = %v, %v", ok, err)
	}

	// No git source means feedback is intentionally absent, not an error.
	(&PreviewFeedback{Logger: logger}).Notify(context.Background(), app, preview, "queued")
}

type previewPromotionFake struct {
	live       int64
	superseded []int64
	running    []int64
	enqueued   int
	status     store.PreviewStatus
}

func (f *previewPromotionFake) EnqueueJob(context.Context, store.EnqueueJobParams) (store.Job, error) {
	f.enqueued++
	return store.Job{ID: int64(f.enqueued)}, nil
}
func (*previewPromotionFake) GetJobByIdempotencyKey(context.Context, *string) (store.Job, error) {
	return store.Job{}, errors.New("unused")
}
func (f *previewPromotionFake) CountLivePreviewsForApplication(context.Context, int64) (int64, error) {
	return f.live, nil
}
func (*previewPromotionFake) GetDestinationByID(context.Context, int64) (store.Destination, error) {
	return store.Destination{ServerID: 9}, nil
}
func (*previewPromotionFake) CreateDeployment(context.Context, store.CreateDeploymentParams) (store.Deployment, error) {
	return store.Deployment{ID: 7}, nil
}
func (f *previewPromotionFake) SupersedeObsoletePreviewDeployments(context.Context, store.SupersedeObsoletePreviewDeploymentsParams) ([]int64, error) {
	return f.superseded, nil
}
func (*previewPromotionFake) CancelJobsForDeployments(context.Context, []int64) error { return nil }
func (f *previewPromotionFake) ListCancellablePreviewDeploymentIDs(context.Context, store.ListCancellablePreviewDeploymentIDsParams) ([]int64, error) {
	return f.running, nil
}
func (*previewPromotionFake) RequestDeploymentJobCancel(context.Context, int64) (int64, error) {
	return 1, nil
}
func (f *previewPromotionFake) SetPreviewStatus(_ context.Context, p store.SetPreviewStatusParams) error {
	f.status = p.Status
	return nil
}

func TestPreviewPromotionCapacityCancellationAndDestroy(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	limit := int32(2)
	app := store.GetApplicationByIDRow{
		Resource: store.Resource{
			ID: 1, Uuid: mustUUID(t, "11111111-1111-4111-8111-111111111111"),
			TeamID: 3, DestinationID: 4, Version: 5,
		},
		Application: store.Application{
			PreviewMaxConcurrent:        &limit,
			PreviewCancelObsoleteBuilds: true,
		},
	}
	sha := "0123456789abcdef"
	preview := store.Preview{
		ID: 2, Uuid: mustUUID(t, "22222222-2222-4222-8222-222222222222"),
		PrID: 42, HeadSha: &sha,
	}

	capped := &previewPromotionFake{live: 2}
	ok, reason, err := TryPromotePreview(context.Background(), capped, logger, app, preview, false)
	if err != nil || ok || !strings.Contains(reason, "concurrency cap") {
		t.Fatalf("capped = %v, %q, %v", ok, reason, err)
	}

	available := &previewPromotionFake{superseded: []int64{10}, running: []int64{11}}
	ok, reason, err = TryPromotePreview(context.Background(), available, logger, app, preview, false)
	if err != nil || !ok || reason != "" || available.enqueued != 1 ||
		available.status != store.PreviewStatusDeploying {
		t.Fatalf("promoted = %v, %q, %v; fake = %#v", ok, reason, err, available)
	}
	if err := EnqueuePreviewDestroy(context.Background(), available, preview); err != nil {
		t.Fatal(err)
	}
	if available.status != store.PreviewStatusDestroying || available.enqueued != 2 {
		t.Fatalf("destroy fake = %#v", available)
	}
}

func TestServerOutputHelpers(t *testing.T) {
	if exitCode(nil) != -1 || stderrOf(nil) != "" {
		t.Fatal("nil SSH result was not handled")
	}
	result := &sshexec.Result{ExitCode: 7, Stderr: "first\nsecond"}
	if exitCode(result) != 7 || stderrOf(result) != "first" {
		t.Fatal("SSH result helpers returned wrong values")
	}
	if got := firstLine("one\ntwo"); got != "one" {
		t.Fatalf("firstLine = %q", got)
	}
	for input, want := range map[string]string{
		"":                                     "done",
		"Total reclaimed space: 12MB\nignored": "Total reclaimed space: 12MB",
		"some other docker output":             "some other docker output",
	} {
		if got := reclaimedLine(input); got != want {
			t.Errorf("reclaimedLine(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPushPolicy(t *testing.T) {
	branch := "release"
	watch := "apps/api/*, shared/"
	app := store.GetApplicationByIDRow{Application: store.Application{
		GitBranch: &branch, AutoDeployEnabled: true, WatchPaths: &watch,
	}}
	base := gitwebhook.Push{Ref: "refs/heads/release", Commit: "abc", Files: []string{"apps/api/main.go"}}
	if got := PushPolicyReason(app, base); got != "" {
		t.Fatalf("accepted push reason = %q", got)
	}
	wrong := base
	wrong.Ref = "refs/heads/main"
	if !strings.Contains(PushPolicyReason(app, wrong), "push on branch") {
		t.Fatal("wrong branch was accepted")
	}
	app.Application.AutoDeployEnabled = false
	if got := PushPolicyReason(app, base); got != "auto_deploy_disabled" {
		t.Fatalf("disabled reason = %q", got)
	}
	app.Application.AutoDeployEnabled = true
	skip := base
	skip.Message = "docs [skip ci]"
	if got := PushPolicyReason(app, skip); got != "skip_ci" {
		t.Fatalf("skip reason = %q", got)
	}
	outside := base
	outside.Files = []string{"docs/readme.md"}
	if !strings.HasPrefix(PushPolicyReason(app, outside), "watch_paths") {
		t.Fatal("unwatched push was accepted")
	}
}

func TestComposeDecisionHelpers(t *testing.T) {
	healthy := compose.ServicePlan{Health: &compose.HealthFlags{
		Test: []string{"CMD", "true"}, Interval: 10 * time.Second,
		Timeout: 2 * time.Second, StartPeriod: 5 * time.Second, Retries: 3,
	}}
	if ok, reason := zeroDowntimeEligibility(healthy, false, false); !ok || reason != "" {
		t.Fatalf("healthy service = (%v, %q)", ok, reason)
	}
	tests := []struct {
		name   string
		plan   compose.ServicePlan
		raw    bool
		image  bool
		reason string
	}{
		{"raw", healthy, true, false, "raw compose"},
		{"opt out", compose.ServicePlan{ZeroDowntimeOptOut: true}, false, true, "zero_downtime"},
		{"host port", compose.ServicePlan{HasHostPorts: true}, false, true, "host port"},
		{"no health", compose.ServicePlan{}, false, false, "healthcheck"},
		{"image health", compose.ServicePlan{}, false, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ok, reason := zeroDowntimeEligibility(tc.plan, tc.raw, tc.image)
			if tc.reason == "" && !ok {
				t.Fatalf("unexpected rejection: %q", reason)
			}
			if tc.reason != "" && (ok || !strings.Contains(reason, tc.reason)) {
				t.Fatalf("decision = (%v, %q), want reason containing %q", ok, reason, tc.reason)
			}
		})
	}
	run := &deploymentRun{}
	if got := run.composeHealthBudget(compose.ServicePlan{}); got != 90 {
		t.Fatalf("default health budget = %d", got)
	}
	if got := run.composeHealthBudget(healthy); got != 71 {
		t.Fatalf("configured health budget = %d", got)
	}
	if got := run.composeHealthBudget(compose.ServicePlan{Health: &compose.HealthFlags{
		Test: []string{"CMD", "true"}, Interval: time.Second, Timeout: time.Second, Retries: 1,
	}}); got != 60 {
		t.Fatalf("minimum health budget = %d", got)
	}
}

func TestRichComposeCreateCommandPreservesSupportedRuntimeOptions(t *testing.T) {
	result, err := compose.Load(context.Background(), compose.Input{
		StackUUID: "11111111-1111-4111-8111-111111111111",
		Content: `services:
  web:
    image: nginx:1.27
    restart: "no"
    labels:
      z.label: last
      a.label: first
    volumes:
      - data:/data:ro
      - ./config:/config:ro
      - type: tmpfs
        target: /tmp
    ports:
      - "127.0.0.1:8080:80/udp"
    deploy:
      resources:
        limits:
          memory: 128M
          cpus: "0.5"
          pids: 100
        reservations:
          memory: 64M
    healthcheck:
      test: ["CMD-SHELL", "wget -qO- http://localhost/health"]
      interval: 10s
      timeout: 2s
      retries: 4
      start_period: 5s
    user: "1000:1000"
    working_dir: /app
    init: true
    read_only: true
    extra_hosts:
      - "host.internal:127.0.0.1"
    stop_grace_period: 15s
    stop_signal: SIGINT
    entrypoint: ["sh", "-c"]
    command: ["echo", "hello world"]
volumes:
  data: {}
`,
	})
	if err != nil || result.Plan == nil {
		t.Fatalf("compose load: %v, findings = %#v", err, result.Findings)
	}
	sp := result.Plan.Services[0]
	sp.OneShot = true
	run := &deploymentRun{dest: store.Destination{Network: "destination"}}
	command := run.composeCreateCommand(
		result.Plan, sp, "/apps/unit", "--label system=true", "/apps/unit/env/web.sh",
		[]string{"SECRET", "PLAIN"}, "nginx:1.27",
		composeCreateOpts{Name: "web-next", Aliases: []string{"web", "web-long"}, ReplaceOld: true},
	)
	for _, fragment := range []string{
		"docker stop -t 30 web-next",
		"--restart no",
		"--network-alias web",
		"akerdock.oneshot=true",
		"a.label=first",
		"data:/data:ro",
		"/apps/unit/mounts/config:/config:ro",
		"--tmpfs '/tmp'",
		"127.0.0.1:8080:80/udp",
		"--memory 134217728",
		"--memory-reservation 67108864",
		"--cpus 0.5",
		"--pids-limit 100",
		"--health-cmd",
		"--user '1000:1000'",
		"--workdir '/app'",
		"--init",
		"--read-only",
		"--add-host 'host.internal:127.0.0.1'",
		"--stop-timeout 15",
		"--stop-signal 'SIGINT'",
		"--entrypoint 'sh'",
		"-e SECRET",
		"hello world",
	} {
		if !strings.Contains(command, fragment) {
			t.Errorf("command misses %q:\n%s", fragment, command)
		}
	}

	sp.Health = &compose.HealthFlags{Disable: true}
	disabled := run.composeCreateCommand(
		result.Plan, sp, "/apps/unit", "", "/tmp/env", nil, "nginx",
		composeCreateOpts{Name: "web"},
	)
	if !strings.Contains(disabled, "--no-healthcheck") {
		t.Fatalf("disabled healthcheck command = %s", disabled)
	}
}

func TestAdoptionAndGenericHelpers(t *testing.T) {
	if nilIfEmpty("") != nil || nilIfEmpty("x") == nil {
		t.Fatal("nilIfEmpty failed")
	}
	if !isUniqueViolationErr(&pgconn.PgError{Code: "23505"}) {
		t.Fatal("unique violation not recognized")
	}
	if isUniqueViolationErr(errors.New("duplicate")) {
		t.Fatal("plain error recognized as unique violation")
	}
	if ptrOf(42) == nil || *ptrOf(42) != 42 {
		t.Fatal("generic ptr helper failed")
	}
	if ptr("x") == nil || *ptr("x") != "x" || deref(nil) != "unknown" || deref(ptr("x")) != "x" {
		t.Fatal("webhook pointer helpers failed")
	}
}

func mustUUID(t *testing.T, raw string) pgtype.UUID {
	t.Helper()
	var u pgtype.UUID
	if err := u.Scan(raw); err != nil {
		t.Fatal(err)
	}
	return u
}
