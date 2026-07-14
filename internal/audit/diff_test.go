package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

// The audit table is append-only, kept forever, and exportable. A secret
// written into it is a second copy of that secret — so the diff must report
// that a sensitive field changed, and never what it changed to.
func TestDiffRedactsSensitiveValues(t *testing.T) {
	before := map[string]any{
		"name":        "web",
		"value":       "old-secret",
		"password":    "hunter2",
		"ports":       "80",
		"private_key": "-----BEGIN KEY-----",
	}
	after := map[string]any{
		"name":        "web-2",
		"value":       "new-secret",
		"password":    "hunter2", // unchanged
		"ports":       "80",      // unchanged
		"private_key": "-----BEGIN OTHER KEY-----",
	}

	diff := Diff(before, after)
	raw, err := json.Marshal(diff)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(raw)

	// Not a single secret, old or new, in any form.
	for _, secret := range []string{"old-secret", "new-secret", "hunter2", "BEGIN KEY", "BEGIN OTHER KEY"} {
		if strings.Contains(encoded, secret) {
			t.Errorf("the audit diff leaked %q: %s", secret, encoded)
		}
	}
	// The non-sensitive change is fully reported — that is the point of a diff.
	if !strings.Contains(encoded, "web-2") || !strings.Contains(encoded, `"web"`) {
		t.Errorf("the name change was not recorded: %s", encoded)
	}
	// The sensitive change IS reported as having happened.
	if _, ok := diff["value"]; !ok {
		t.Error("a changed secret must still appear as changed")
	}
	if _, ok := diff["private_key"]; !ok {
		t.Error("a changed private key must still appear as changed")
	}
	// Unchanged fields produce no noise.
	if _, ok := diff["ports"]; ok {
		t.Error("an unchanged field must not appear in the diff")
	}
	if _, ok := diff["password"]; ok {
		t.Error("an unchanged secret must not appear in the diff")
	}
}

func TestDiffEmpty(t *testing.T) {
	same := map[string]any{"name": "web"}
	if d := Diff(same, same); len(d) != 0 {
		t.Errorf("an identical pair must produce no diff, got %v", d)
	}
}
