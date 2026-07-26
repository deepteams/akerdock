package logredact

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func newLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(Wrap(slog.NewJSONHandler(buf, nil)))
}

func TestRedactsSensitiveValues(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf)
	log.Info("login",
		slog.String("password", "hunter2"),
		slog.String("token", "akd_supersecret"),
		slog.String("client_secret", "oauthsecret"),
		slog.String("db_password", "pgpw"),
		slog.String("email", "user@example.test"), // not sensitive
		slog.String("token_uuid", "11111111-1111-1111-1111-111111111111"), // identifier, not the secret
	)

	out := buf.String()
	for _, leaked := range []string{"hunter2", "akd_supersecret", "oauthsecret", "pgpw"} {
		if strings.Contains(out, leaked) {
			t.Errorf("log leaked a secret value %q: %s", leaked, out)
		}
	}
	if strings.Count(out, Placeholder) < 4 {
		t.Errorf("expected 4 redactions, got: %s", out)
	}
	// Non-sensitive fields survive.
	if !strings.Contains(out, "user@example.test") {
		t.Errorf("redacted a non-sensitive value: %s", out)
	}
	if !strings.Contains(out, "11111111-1111-1111-1111-111111111111") {
		t.Errorf("token_uuid is an identifier and must not be redacted: %s", out)
	}
}

func TestRedactsInsideGroups(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf)
	log.Info("event", slog.Group("actor", slog.String("secret", "nested-secret"), slog.String("name", "alice")))

	out := buf.String()
	if strings.Contains(out, "nested-secret") {
		t.Errorf("a secret nested in a group leaked: %s", out)
	}
	if !strings.Contains(out, "alice") {
		t.Errorf("a non-sensitive nested value was dropped: %s", out)
	}
}

func TestRedactsWithAttrs(t *testing.T) {
	var buf bytes.Buffer
	log := newLogger(&buf).With(slog.String("api_key", "preset-secret"))
	log.Info("call")

	if strings.Contains(buf.String(), "preset-secret") {
		t.Errorf("a preset (WithAttrs) secret leaked: %s", buf.String())
	}
}

func TestNonSensitiveRecordIsIntact(t *testing.T) {
	var buf bytes.Buffer
	newLogger(&buf).Info("hello", slog.Int("count", 3))
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("log is not valid JSON: %v", err)
	}
	if m["msg"] != "hello" || m["count"].(float64) != 3 {
		t.Errorf("record was altered: %v", m)
	}
	_ = context.Background()
}
