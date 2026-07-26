package gitwebhook

import (
	"fmt"
	"testing"
)

func TestParsePullRequestGitHub(t *testing.T) {
	body := []byte(`{
		"action": "opened", "number": 7,
		"pull_request": {
			"draft": false, "merged": false, "title": "Add feature",
			"labels": [{"name": "preview"}, {"name": "bug"}],
			"head": {"ref": "feat/x", "sha": "abc123", "repo": {"id": 11}},
			"base": {"repo": {"id": 11, "full_name": "org/app"}}
		},
		"repository": {"full_name": "org/app"}
	}`)
	ev, err := ParsePullRequest(GitHub, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Action != "opened" || ev.Number != 7 || ev.HeadSHA != "abc123" || ev.HeadRef != "feat/x" {
		t.Fatalf("bad event: %+v", ev)
	}
	if ev.IsFork {
		t.Fatal("same repo ids must not be a fork")
	}
	if ev.RepoReference != "org/app" {
		t.Fatalf("repo reference: %q", ev.RepoReference)
	}
	if !ev.HasLabel("preview") || ev.HasLabel("nope") {
		t.Fatalf("labels: %v", ev.Labels)
	}
}

func TestParsePullRequestGitHubFork(t *testing.T) {
	body := []byte(`{"action":"opened","number":1,"pull_request":{
		"head":{"ref":"f","sha":"s","repo":{"id":99}},
		"base":{"repo":{"id":11,"full_name":"org/app"}}}}`)
	ev, err := ParsePullRequest(GitHub, body)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.IsFork {
		t.Fatal("different repo ids must be a fork")
	}
}

func TestParsePullRequestGiteaSynchronized(t *testing.T) {
	// Gitea says "synchronized" (with a d) where GitHub says "synchronize".
	body := []byte(`{"action":"synchronized","number":3,"pull_request":{
		"title":"x","head":{"ref":"b","sha":"s2","repo":{"id":5}},
		"base":{"repo":{"id":5,"full_name":"o/r"}}}}`)
	ev, err := ParsePullRequest(Gitea, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Action != "synchronize" {
		t.Fatalf("action not normalized: %q", ev.Action)
	}
}

func TestParsePullRequestGiteaWIPTitle(t *testing.T) {
	body := []byte(`{"action":"opened","number":3,"pull_request":{
		"title":"WIP: not ready","head":{"ref":"b","sha":"s","repo":{"id":5}},
		"base":{"repo":{"id":5,"full_name":"o/r"}}}}`)
	ev, err := ParsePullRequest(Gitea, body)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Draft {
		t.Fatal("a WIP: title is a draft")
	}
}

func TestParsePullRequestGitLab(t *testing.T) {
	base := `{"object_kind":"merge_request","project":{"id":42},
		"labels":[{"title":"preview"}],
		"object_attributes":{"iid":9,"action":%q,%s
			"source_branch":"feat","source_project_id":42,"target_project_id":42,
			"last_commit":{"id":"deadbeef"}}}`

	cases := []struct {
		glAction string
		extra    string
		want     string
		merged   bool
	}{
		{"open", "", "opened", false},
		{"reopen", "", "reopened", false},
		{"close", "", "closed", false},
		{"merge", "", "closed", true},
		{"update", `"oldrev":"aaa",`, "synchronize", false},
		{"update", "", "ignored", false},
	}
	for _, c := range cases {
		body := fmt.Appendf(nil, base, c.glAction, c.extra)
		ev, err := ParsePullRequest(GitLab, body)
		if err != nil {
			t.Fatalf("%s: %v", c.glAction, err)
		}
		if ev.Action != c.want {
			t.Fatalf("%s: got action %q, want %q", c.glAction, ev.Action, c.want)
		}
		if ev.Merged != c.merged {
			t.Fatalf("%s: merged=%v", c.glAction, ev.Merged)
		}
		if ev.Number != 9 || ev.RepoReference != "42" {
			t.Fatalf("%s: identity %+v", c.glAction, ev)
		}
	}
}

func TestParsePullRequestGitLabLabelUpdate(t *testing.T) {
	// An update with no new commit but a label change re-evaluates the label
	// opt-in (§20.4.7) instead of being dropped.
	body := []byte(`{"object_kind":"merge_request","project":{"id":1},
		"changes":{"labels":{}},
		"object_attributes":{"iid":2,"action":"update","source_branch":"b",
			"source_project_id":1,"target_project_id":1,"last_commit":{"id":"s"}}}`)
	ev, err := ParsePullRequest(GitLab, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Action != "labeled" {
		t.Fatalf("got %q, want labeled", ev.Action)
	}
}

func TestParsePullRequestGitLabFork(t *testing.T) {
	body := []byte(`{"object_kind":"merge_request","project":{"id":1},
		"object_attributes":{"iid":2,"action":"open","source_branch":"b",
			"source_project_id":77,"target_project_id":1,"last_commit":{"id":"s"}}}`)
	ev, err := ParsePullRequest(GitLab, body)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.IsFork {
		t.Fatal("source != target project must be a fork")
	}
}

func TestParsePullRequestGitLabDraft(t *testing.T) {
	body := []byte(`{"object_kind":"merge_request","project":{"id":1},
		"object_attributes":{"iid":2,"action":"open","work_in_progress":true,
			"source_project_id":1,"target_project_id":1,"last_commit":{"id":"s"}}}`)
	ev, err := ParsePullRequest(GitLab, body)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.Draft {
		t.Fatal("work_in_progress is a draft")
	}
}

func TestParseCommentGitHub(t *testing.T) {
	body := []byte(`{"action":"created",
		"issue":{"number":4,"pull_request":{}},
		"comment":{"body":"/deploy\nplease","user":{"id":8,"login":"alice"}},
		"repository":{"full_name":"o/r"}}`)
	ev, err := ParseComment(GitHub, body)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.OnPullRequest || ev.Number != 4 || ev.AuthorUsername != "alice" {
		t.Fatalf("bad event: %+v", ev)
	}
	if ev.Command() != "deploy" {
		t.Fatalf("command: %q", ev.Command())
	}
}

func TestParseCommentGitHubPlainIssue(t *testing.T) {
	// A comment on a plain issue (no pull_request key) carries no command.
	body := []byte(`{"action":"created","issue":{"number":4},
		"comment":{"body":"/deploy","user":{"login":"a"}},
		"repository":{"full_name":"o/r"}}`)
	ev, err := ParseComment(GitHub, body)
	if err != nil {
		t.Fatal(err)
	}
	if ev.OnPullRequest {
		t.Fatal("a plain issue comment is not a PR comment")
	}
}

func TestParseCommentGitLab(t *testing.T) {
	body := []byte(`{"object_kind":"note",
		"user":{"id":12,"username":"bob"},
		"project":{"id":42},
		"object_attributes":{"note":"/destroy","notable_type":"MergeRequest"},
		"merge_request":{"iid":9}}`)
	ev, err := ParseComment(GitLab, body)
	if err != nil {
		t.Fatal(err)
	}
	if !ev.OnPullRequest || ev.Number != 9 || ev.AuthorID != 12 || ev.RepoReference != "42" {
		t.Fatalf("bad event: %+v", ev)
	}
	if ev.Command() != "destroy" {
		t.Fatalf("command: %q", ev.Command())
	}
}

func TestCommandFirstLineOnly(t *testing.T) {
	// A command quoted mid-text must not fire (§2.7d).
	cases := map[string]string{
		"/deploy":            "deploy",
		"/destroy":           "destroy",
		"/rebuild":           "rebuild",
		"/keep":              "keep",
		"  /keep  ":          "keep",
		"/deploy\nand more":  "deploy",
		"please run /deploy": "",
		"quote:\n/deploy":    "",
		"/deployment":        "",
		"/keeper":            "",
		"/deploy now":        "",
		"":                   "",
	}
	for body, want := range cases {
		got := CommentEvent{Body: body}.Command()
		if got != want {
			t.Errorf("%q: got %q, want %q", body, got, want)
		}
	}
}

func TestEventTypePredicates(t *testing.T) {
	if !IsPullRequestEvent(GitHub, "pull_request") || !IsPullRequestEvent(Gitea, "pull_request") {
		t.Fatal("github/gitea pull_request")
	}
	if !IsPullRequestEvent(GitLab, "Merge Request Hook") {
		t.Fatal("gitlab Merge Request Hook")
	}
	if IsPullRequestEvent(GitLab, "pull_request") || IsPullRequestEvent(GitHub, "push") {
		t.Fatal("false positives")
	}
	if !IsCommentEvent(GitHub, "issue_comment") || !IsCommentEvent(GitLab, "Note Hook") || !IsCommentEvent(Gitea, "issue_comment") {
		t.Fatal("comment predicates")
	}
	if IsPullRequestEvent("unknown", "pull_request") || IsCommentEvent("unknown", "issue_comment") {
		t.Fatal("unknown providers must not match event predicates")
	}
}

func TestPullRequestFallbacksAndValidation(t *testing.T) {
	ev, err := ParsePullRequest(GitHub, []byte(`{
		"action":"opened",
		"pull_request":{
			"number":12,
			"title":"Draft: pending",
			"head":{"repo":{"id":1}},
			"base":{"repo":{"id":1}}
		},
		"repository":{"full_name":"fallback/repo"}
	}`))
	if err != nil || ev.Number != 12 || ev.RepoReference != "fallback/repo" || !ev.Draft {
		t.Fatalf("GitHub fallbacks were not applied: %+v, %v", ev, err)
	}

	for _, tc := range []struct {
		provider Provider
		body     []byte
	}{
		{GitHub, []byte(`{`)},
		{GitLab, []byte(`{"object_kind":"push"}`)},
		{"unknown", []byte(`{}`)},
	} {
		if _, err := ParsePullRequest(tc.provider, tc.body); err == nil {
			t.Errorf("%s malformed/unsupported pull request should fail", tc.provider)
		}
	}
}

func TestCommentValidation(t *testing.T) {
	for _, tc := range []struct {
		provider Provider
		body     []byte
	}{
		{GitHub, []byte(`{`)},
		{GitLab, []byte(`{"object_kind":"push"}`)},
		{"unknown", []byte(`{}`)},
	} {
		if _, err := ParseComment(tc.provider, tc.body); err == nil {
			t.Errorf("%s malformed/unsupported comment should fail", tc.provider)
		}
	}
}
