package gitwebhook

import (
	"encoding/json"
	"fmt"
)

// ParsePush normalizes a push payload. The three forges disagree on the shape
// of everything except the ref, so the differences are absorbed here rather
// than leaking into the deployment engine.
func ParsePush(p Provider, body []byte) (Push, error) {
	switch p {
	case GitHub, Gitea:
		// Gitea mirrors GitHub's push payload.
		var raw struct {
			Ref   string `json:"ref"`
			After string `json:"after"`
			Head  struct {
				Message string `json:"message"`
			} `json:"head_commit"`
			Commits []struct {
				Added    []string `json:"added"`
				Removed  []string `json:"removed"`
				Modified []string `json:"modified"`
			} `json:"commits"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return Push{}, fmt.Errorf("unparsable push payload")
		}
		push := Push{Ref: raw.Ref, Commit: raw.After, Message: raw.Head.Message}
		for _, c := range raw.Commits {
			push.Files = append(push.Files, c.Added...)
			push.Files = append(push.Files, c.Removed...)
			push.Files = append(push.Files, c.Modified...)
		}
		return push, nil

	case GitLab:
		var raw struct {
			Ref         string `json:"ref"`
			After       string `json:"after"`
			CheckoutSha string `json:"checkout_sha"`
			Commits     []struct {
				Message  string   `json:"message"`
				Added    []string `json:"added"`
				Removed  []string `json:"removed"`
				Modified []string `json:"modified"`
			} `json:"commits"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			return Push{}, fmt.Errorf("unparsable push payload")
		}
		push := Push{Ref: raw.Ref, Commit: raw.After}
		if push.Commit == "" {
			push.Commit = raw.CheckoutSha
		}
		for _, c := range raw.Commits {
			push.Files = append(push.Files, c.Added...)
			push.Files = append(push.Files, c.Removed...)
			push.Files = append(push.Files, c.Modified...)
		}
		// GitLab has no head_commit: the last commit of the list is the head.
		if n := len(raw.Commits); n > 0 {
			push.Message = raw.Commits[n-1].Message
		}
		return push, nil

	default:
		return Push{}, fmt.Errorf("unsupported provider")
	}
}
