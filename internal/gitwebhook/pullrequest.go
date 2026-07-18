package gitwebhook

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// PullRequestEvent is what the preview lifecycle needs out of a PR/MR
// delivery, whichever forge sent it (§20.4, protocols §2.4/§4.3/§6.2). The
// forges disagree on action names, draft flags and fork detection; the
// differences are absorbed here.
type PullRequestEvent struct {
	// Action is normalized to the GitHub vocabulary: opened, synchronize,
	// reopened, ready_for_review, closed, labeled, unlabeled — or "ignored"
	// for deliveries that carry no lifecycle meaning (e.g. a GitLab update
	// with no new commit and no label change).
	Action  string
	Number  int
	HeadRef string
	HeadSHA string
	Draft   bool
	Merged  bool
	IsFork  bool
	// Labels are the PR's current labels — the substrate of the opt-in
	// label control (§20.4.7).
	Labels []string
	// RepoReference is the handle feedback calls need (§20.4.6): the GitLab
	// project id, or the owner/repo full name for GitHub and Gitea.
	RepoReference string
}

// HasLabel reports whether the PR carries the given label.
func (e PullRequestEvent) HasLabel(label string) bool {
	return slices.Contains(e.Labels, label)
}

// githubPREvent is the GitHub shape; Gitea mirrors it.
type githubPREvent struct {
	Action      string `json:"action"`
	Number      int    `json:"number"`
	PullRequest struct {
		Number int    `json:"number"`
		Draft  bool   `json:"draft"`
		Merged bool   `json:"merged"`
		Title  string `json:"title"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		Head struct {
			Ref  string `json:"ref"`
			SHA  string `json:"sha"`
			Repo struct {
				ID int64 `json:"id"`
			} `json:"repo"`
		} `json:"head"`
		Base struct {
			Repo struct {
				ID       int64  `json:"id"`
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"base"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

// ParsePullRequest normalizes a pull_request / Merge Request Hook payload.
func ParsePullRequest(p Provider, body []byte) (PullRequestEvent, error) {
	switch p {
	case GitHub, Gitea:
		var raw githubPREvent
		if err := json.Unmarshal(body, &raw); err != nil {
			return PullRequestEvent{}, fmt.Errorf("unparsable pull_request payload")
		}
		ev := PullRequestEvent{
			Action:  raw.Action,
			Number:  raw.Number,
			HeadRef: raw.PullRequest.Head.Ref,
			HeadSHA: raw.PullRequest.Head.SHA,
			Draft:   raw.PullRequest.Draft,
			Merged:  raw.PullRequest.Merged,
			IsFork: raw.PullRequest.Head.Repo.ID != 0 &&
				raw.PullRequest.Head.Repo.ID != raw.PullRequest.Base.Repo.ID,
			RepoReference: raw.PullRequest.Base.Repo.FullName,
		}
		if ev.Number == 0 {
			ev.Number = raw.PullRequest.Number
		}
		if ev.RepoReference == "" {
			ev.RepoReference = raw.Repository.FullName
		}
		for _, l := range raw.PullRequest.Labels {
			ev.Labels = append(ev.Labels, l.Name)
		}
		// Gitea says "synchronized" where GitHub says "synchronize" (§6.2);
		// older Gitea flags drafts by title prefix only.
		if ev.Action == "synchronized" {
			ev.Action = "synchronize"
		}
		if title := raw.PullRequest.Title; strings.HasPrefix(title, "WIP:") || strings.HasPrefix(title, "Draft:") {
			ev.Draft = true
		}
		return ev, nil

	case GitLab:
		var raw struct {
			ObjectKind string `json:"object_kind"`
			Project    struct {
				ID int64 `json:"id"`
			} `json:"project"`
			ObjectAttributes struct {
				IID             int    `json:"iid"`
				Action          string `json:"action"`
				OldRev          string `json:"oldrev"`
				SourceBranch    string `json:"source_branch"`
				SourceProjectID int64  `json:"source_project_id"`
				TargetProjectID int64  `json:"target_project_id"`
				WorkInProgress  bool   `json:"work_in_progress"`
				Draft           bool   `json:"draft"`
				LastCommit      struct {
					ID string `json:"id"`
				} `json:"last_commit"`
			} `json:"object_attributes"`
			Labels []struct {
				Title string `json:"title"`
			} `json:"labels"`
			Changes struct {
				Labels *struct{} `json:"labels"`
			} `json:"changes"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || raw.ObjectKind != "merge_request" {
			return PullRequestEvent{}, fmt.Errorf("unparsable merge_request payload")
		}
		ev := PullRequestEvent{
			Number:  raw.ObjectAttributes.IID,
			HeadRef: raw.ObjectAttributes.SourceBranch,
			HeadSHA: raw.ObjectAttributes.LastCommit.ID,
			Draft:   raw.ObjectAttributes.Draft || raw.ObjectAttributes.WorkInProgress,
			IsFork: raw.ObjectAttributes.SourceProjectID != 0 &&
				raw.ObjectAttributes.SourceProjectID != raw.ObjectAttributes.TargetProjectID,
			RepoReference: strconv.FormatInt(raw.Project.ID, 10),
		}
		for _, l := range raw.Labels {
			ev.Labels = append(ev.Labels, l.Title)
		}
		switch raw.ObjectAttributes.Action {
		case "open":
			ev.Action = "opened"
		case "reopen":
			ev.Action = "reopened"
		case "close":
			ev.Action = "closed"
		case "merge":
			ev.Action, ev.Merged = "closed", true
		case "update":
			switch {
			// A new commit is the only update that redeploys (§4.3): GitLab
			// fires the same hook for title edits, assignees, labels…
			case raw.ObjectAttributes.OldRev != "":
				ev.Action = "synchronize"
			case raw.Changes.Labels != nil:
				ev.Action = "labeled"
			default:
				ev.Action = "ignored"
			}
		default:
			ev.Action = "ignored"
		}
		return ev, nil

	default:
		return PullRequestEvent{}, fmt.Errorf("unsupported provider")
	}
}

// CommentEvent is a PR/MR comment, normalized — the carrier of the /deploy
// and /destroy commands and of fork approvals (§20.4.7, protocols §2.7d).
type CommentEvent struct {
	Number int
	Body   string
	// Author identifies the comment author for the server-side rights check:
	// username for GitHub/Gitea (their permission APIs key on it), numeric id
	// for GitLab (its membership API keys on it).
	AuthorUsername string
	AuthorID       int64
	RepoReference  string
	// OnPullRequest distinguishes a PR comment from a plain issue comment —
	// commands on plain issues mean nothing.
	OnPullRequest bool
}

// Command extracts the command when the FIRST line is exactly /deploy or
// /destroy (protocols §2.7d — a command quoted mid-text must not fire).
func (e CommentEvent) Command() string {
	first := e.Body
	if i := strings.IndexAny(first, "\r\n"); i >= 0 {
		first = first[:i]
	}
	switch strings.TrimSpace(first) {
	case "/deploy":
		return "deploy"
	case "/destroy":
		return "destroy"
	default:
		return ""
	}
}

// ParseComment normalizes an issue_comment (GitHub/Gitea) or Note Hook
// (GitLab) payload. Only comment creations carry commands: edits and
// deletions are reported as !OnPullRequest.
func ParseComment(p Provider, body []byte) (CommentEvent, error) {
	switch p {
	case GitHub, Gitea:
		var raw struct {
			Action string `json:"action"`
			Issue  struct {
				Number      int       `json:"number"`
				PullRequest *struct{} `json:"pull_request"`
			} `json:"issue"`
			Comment struct {
				Body string `json:"body"`
				User struct {
					ID    int64  `json:"id"`
					Login string `json:"login"`
				} `json:"user"`
			} `json:"comment"`
			Repository struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return CommentEvent{}, fmt.Errorf("unparsable comment payload")
		}
		return CommentEvent{
			Number:         raw.Issue.Number,
			Body:           raw.Comment.Body,
			AuthorUsername: raw.Comment.User.Login,
			AuthorID:       raw.Comment.User.ID,
			RepoReference:  raw.Repository.FullName,
			OnPullRequest:  raw.Action == "created" && raw.Issue.PullRequest != nil,
		}, nil

	case GitLab:
		var raw struct {
			ObjectKind string `json:"object_kind"`
			User       struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			} `json:"user"`
			Project struct {
				ID int64 `json:"id"`
			} `json:"project"`
			ObjectAttributes struct {
				Note         string `json:"note"`
				NoteableType string `json:"noteable_type"`
			} `json:"object_attributes"`
			MergeRequest struct {
				IID int `json:"iid"`
			} `json:"merge_request"`
		}
		if err := json.Unmarshal(body, &raw); err != nil || raw.ObjectKind != "note" {
			return CommentEvent{}, fmt.Errorf("unparsable note payload")
		}
		return CommentEvent{
			Number:         raw.MergeRequest.IID,
			Body:           raw.ObjectAttributes.Note,
			AuthorUsername: raw.User.Username,
			AuthorID:       raw.User.ID,
			RepoReference:  strconv.FormatInt(raw.Project.ID, 10),
			OnPullRequest:  raw.ObjectAttributes.NoteableType == "MergeRequest",
		}, nil

	default:
		return CommentEvent{}, fmt.Errorf("unsupported provider")
	}
}

// IsPullRequestEvent reports whether this event type carries a PR/MR
// lifecycle payload for this provider.
func IsPullRequestEvent(p Provider, eventType string) bool {
	switch p {
	case GitHub, Gitea:
		return eventType == "pull_request"
	case GitLab:
		return eventType == "Merge Request Hook"
	default:
		return false
	}
}

// IsCommentEvent reports whether this event type carries a comment payload.
func IsCommentEvent(p Provider, eventType string) bool {
	switch p {
	case GitHub, Gitea:
		return eventType == "issue_comment"
	case GitLab:
		return eventType == "Note Hook"
	default:
		return false
	}
}
