package notify

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/deepteams/akerdock/internal/store"
)

// The taxonomy decides what may be debounced or held for quiet hours, so a
// misclassified failure is a silenced outage (ADR-019).
func TestSeverityOf(t *testing.T) {
	cases := map[string]Severity{
		"deployment.succeeded.v1": SeverityInfo,
		"deployment.failed.v1":    SeverityCritical,
		"deployment.cancelled.v1": SeverityWarning,
		"server.unreachable.v1":   SeverityCritical,
		"backup.failed.v1":        SeverityCritical,
		"backup.partial.v1":       SeverityWarning,
		"job.dead_letter.v1":      SeverityCritical,
		"certificate.expiring.v1": SeverityWarning,
		"application.created.v1":  SeverityInfo,
	}
	for event, want := range cases {
		if got := SeverityOf(event); got != want {
			t.Errorf("SeverityOf(%q) = %s, want %s", event, got, want)
		}
	}
}

func quietRule(start, end string) store.MatchNotificationRulesRow {
	parse := func(s string) pgtype.Time {
		var t pgtype.Time
		_ = t.Scan(s)
		return t
	}
	return store.MatchNotificationRulesRow{
		QuietHoursStart: parse(start), QuietHoursEnd: parse(end),
	}
}

// A quiet window that wraps around midnight (22:00 → 07:00) is the normal
// case: an on-call night is not "from 00:00 to 07:00".
func TestInQuietHoursWrapsMidnight(t *testing.T) {
	rule := quietRule("22:00:00", "07:00:00")
	at := func(h, m int) time.Time { return time.Date(2026, 7, 11, h, m, 0, 0, time.Local) }

	for _, tc := range []struct {
		when time.Time
		want bool
	}{
		{at(23, 30), true},  // after the start, before midnight
		{at(2, 0), true},    // after midnight, before the end
		{at(6, 59), true},   // last quiet minute
		{at(7, 0), false},   // the window is half-open: 07:00 is awake
		{at(12, 0), false},  // broad daylight
		{at(21, 59), false}, // one minute before the window
	} {
		if got := inQuietHours(rule, tc.when); got != tc.want {
			t.Errorf("inQuietHours(22:00→07:00, %s) = %v, want %v", tc.when.Format("15:04"), got, tc.want)
		}
	}
}

// A window inside a single day must not be treated as wrapping.
func TestInQuietHoursSameDay(t *testing.T) {
	rule := quietRule("09:00:00", "18:00:00")
	at := func(h int) time.Time { return time.Date(2026, 7, 11, h, 0, 0, 0, time.Local) }
	if !inQuietHours(rule, at(12)) {
		t.Error("noon must be inside 09:00→18:00")
	}
	if inQuietHours(rule, at(3)) {
		t.Error("3am must be outside 09:00→18:00")
	}
}

// A rule with no window never suppresses.
func TestNoQuietHours(t *testing.T) {
	if inQuietHours(store.MatchNotificationRulesRow{}, time.Now()) {
		t.Error("a rule without quiet hours must never suppress")
	}
}

func TestEventText(t *testing.T) {
	e := Event{Type: "deployment.failed.v1", Severity: "critical", Resource: "abc", Suppressed: 12}
	got := e.Text()
	for _, want := range []string{"🔴", "Deployment failed", "abc", "and 12 similar events"} {
		if !strings.Contains(got, want) {
			t.Errorf("Text() = %q, missing %q", got, want)
		}
	}

	// A payload-rich event names the resource and the actionable facts.
	rich := Event{Type: "deployment.failed.v1", Severity: "critical", Resource: "uuid-x", Payload: map[string]any{
		"name": "varuna", "pr_id": float64(8), "commit_sha": "abcdef1234567890", "error": "the health check did not turn healthy\nmore",
	}}
	rt := rich.Text()
	for _, want := range []string{"varuna", "PR #8", "abcdef12", "the health check did not turn healthy"} {
		if !strings.Contains(rt, want) {
			t.Errorf("rich Text() = %q, missing %q", rt, want)
		}
	}
	if strings.Contains(rt, "uuid-x") {
		t.Errorf("rich Text() should prefer the name over the uuid: %q", rt)
	}
	if strings.Contains(got, ".v1") {
		t.Errorf("the version suffix must not reach a human message: %q", got)
	}

	// A preview event carries `fqdn`, not `url`: plain-text transports surface
	// it, while Slack keeps it exclusively on the Open button and uses the
	// visible fields for the author and branch.
	prev := Event{Type: "application.preview.updated.v1", Severity: "info", Payload: map[string]any{
		"name": "varuna", "pr_id": float64(8), "fqdn": "varuna-pr8.ad.example.com",
		"commit_author": "Ada Lovelace", "branch": "feat/analytical-engine",
	}}
	if pt := prev.Text(); !strings.Contains(pt, "https://varuna-pr8.ad.example.com") {
		t.Errorf("preview Text() missing url: %q", pt)
	}
	slack := slackMessage(prev)
	if fallback := fmt.Sprint(slack["text"]); strings.Contains(fallback, "https://") {
		t.Errorf("Slack fallback exposes the raw URL: %q", fallback)
	}
	att, _ := slack["attachments"].([]map[string]any)
	if len(att) == 0 {
		t.Fatal("Slack message has no attachment")
	}
	blocks, _ := att[0]["blocks"].([]map[string]any)
	var fieldsText, openURL string
	for _, block := range blocks {
		switch block["type"] {
		case "section":
			if fields, ok := block["fields"].([]map[string]any); ok {
				fieldsText += fmt.Sprint(fields)
			}
		case "actions":
			if elements, ok := block["elements"].([]map[string]any); ok && len(elements) > 0 {
				openURL = fmt.Sprint(elements[0]["url"])
			}
		}
	}
	for _, want := range []string{"*Author:*", "Ada Lovelace", "*Branch:*", "feat/analytical-engine"} {
		if !strings.Contains(fieldsText, want) {
			t.Errorf("Slack fields = %q, missing %q", fieldsText, want)
		}
	}
	if strings.Contains(fieldsText, "*URL:*") || strings.Contains(fieldsText, "https://") {
		t.Errorf("Slack fields expose the raw URL: %q", fieldsText)
	}
	if want := "https://varuna-pr8.ad.example.com"; openURL != want {
		t.Errorf("Open button URL = %q, want %q", openURL, want)
	}
}

func TestValidateConfig(t *testing.T) {
	if err := ValidateConfig("webhook", Config{URL: "https://hooks.example/x"}); err != nil {
		t.Errorf("a valid webhook was refused: %v", err)
	}
	if err := ValidateConfig("webhook", Config{URL: "not-a-url"}); err == nil {
		t.Error("a malformed URL must be refused")
	}
	if err := ValidateConfig("telegram", Config{URL: "https://x.example"}); err == nil {
		t.Error("an unimplemented channel must be refused, not silently accepted")
	}
}
