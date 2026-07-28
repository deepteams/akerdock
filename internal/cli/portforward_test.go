package cli

import (
	"strings"
	"testing"
	"time"
)

// A tunnel that dies in silence reads as a bug in AkerDock, and the
// developer's next move is to look for a way around the platform rather than
// back into it. Every automatic close must say why AND what to do.
func TestCloseMessageIsActionable(t *testing.T) {
	for reason, want := range map[string]string{
		"idle_timeout":  "rerun",
		"grant_expired": "request access again",
		"revoked":       "administrator",
		"max_duration":  "rerun",
		"disconnect":    "dropped",
	} {
		got := closeMessage(reason)
		if got == "" {
			t.Errorf("%q produced no message at all", reason)
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("closeMessage(%q) = %q, want it to mention %q", reason, got, want)
		}
	}
	// Ctrl-C is not an incident: the developer already knows why it stopped.
	if got := closeMessage("user_close"); got != "" {
		t.Errorf("a deliberate close should stay silent, got %q", got)
	}
	// An unknown reason still surfaces rather than vanishing.
	if got := closeMessage("something_new"); got == "" {
		t.Error("an unrecognised reason must still be reported")
	}
}

// The deadline is announced when the tunnel opens, not only when it ends:
// absolute so a long transfer can be planned around it, relative so it
// registers at a glance.
func TestAuthorizedSuffixCarriesBothForms(t *testing.T) {
	if got := authorizedSuffix(nil); got != "" {
		t.Errorf("no deadline should add nothing to the line, got %q", got)
	}
	until := time.Now().Add(3*time.Hour + 47*time.Minute)
	got := authorizedSuffix(&until)
	if !strings.Contains(got, until.Local().Format("15:04")) {
		t.Errorf("%q should carry the absolute time", got)
	}
	if !strings.Contains(got, "3h4") {
		t.Errorf("%q should carry the remaining time", got)
	}
	past := time.Now().Add(-time.Minute)
	if got := authorizedSuffix(&past); !strings.Contains(got, "expired") {
		t.Errorf("a lapsed authorization should say so, got %q", got)
	}
}

func TestHumanDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		45 * time.Minute:            "45m",
		2 * time.Hour:               "2h00",
		2*time.Hour + 5*time.Minute: "2h05",
		90 * time.Minute:            "1h30",
	} {
		if got := humanDuration(d); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
