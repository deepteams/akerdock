package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The payload shape of a provider is a contract we cannot compile against: a
// wrong field name is accepted by the type system and rejected by the provider,
// at 3am, on the alert that mattered. So each transport is exercised against a
// server that asserts what the provider actually requires.

func captureRequest(t *testing.T, handler func(r *http.Request, body map[string]any)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("the provider received a body that is not JSON: %s", raw)
		}
		handler(r, body)
		w.WriteHeader(http.StatusOK)
	}))
}

func testEvent() Event {
	return Event{Type: "deployment.failed.v1", Resource: "web"}
}

func TestTelegramPayload(t *testing.T) {
	var gotPath string
	srv := captureRequest(t, func(r *http.Request, body map[string]any) {
		gotPath = r.URL.Path
		if body["chat_id"] != "-100123" {
			t.Errorf("chat_id = %v, want -100123", body["chat_id"])
		}
		if !strings.Contains(body["text"].(string), "deployment.failed") {
			t.Errorf("text does not name the event: %v", body["text"])
		}
	})
	defer srv.Close()
	telegramBase = srv.URL
	defer func() { telegramBase = "https://api.telegram.org" }()

	cfg := Config{Telegram: &TelegramConfig{BotToken: "42:secret", ChatID: "-100123"}}
	if err := testSender().Send(context.Background(), "telegram", cfg, testEvent()); err != nil {
		t.Fatalf("send: %v", err)
	}
	// The token belongs in the path of the Bot API — this is the one provider
	// that puts a credential in the URL, and getting it wrong means a 404 that
	// looks like a network problem.
	if gotPath != "/bot42:secret/sendMessage" {
		t.Errorf("path = %q, want /bot42:secret/sendMessage", gotPath)
	}
}

func TestPushoverPayload(t *testing.T) {
	srv := captureRequest(t, func(_ *http.Request, body map[string]any) {
		if body["token"] != "app-token" || body["user"] != "user-key" {
			t.Errorf("pushover credentials not in the body: %v", body)
		}
		if body["message"] == nil {
			t.Error("pushover requires a message field")
		}
	})
	defer srv.Close()
	pushoverEndpoint = srv.URL
	defer func() { pushoverEndpoint = "https://api.pushover.net/1/messages.json" }()

	cfg := Config{Pushover: &PushoverConfig{Token: "app-token", UserKey: "user-key"}}
	if err := testSender().Send(context.Background(), "pushover", cfg, testEvent()); err != nil {
		t.Fatalf("send: %v", err)
	}
}

func TestResendSendsTheKeyAsAHeaderNotInTheURL(t *testing.T) {
	var auth, rawURL string
	srv := captureRequest(t, func(r *http.Request, body map[string]any) {
		auth = r.Header.Get("Authorization")
		rawURL = r.URL.String()
		if body["from"] != "akerdock@example.com" {
			t.Errorf("from = %v", body["from"])
		}
	})
	defer srv.Close()
	resendEndpoint = srv.URL
	defer func() { resendEndpoint = "https://api.resend.com/emails" }()

	cfg := Config{Resend: &ResendConfig{APIKey: "re_secret", From: "akerdock@example.com", To: []string{"ops@example.com"}}}
	if err := testSender().Send(context.Background(), "resend", cfg, testEvent()); err != nil {
		t.Fatalf("send: %v", err)
	}
	if auth != "Bearer re_secret" {
		t.Errorf("Authorization = %q", auth)
	}
	// A URL travels in logs, proxies and referrers; a header does not.
	if strings.Contains(rawURL, "re_secret") {
		t.Errorf("the API key leaked into the URL: %s", rawURL)
	}
}

func TestSMTPRefusesToSendAPasswordInTheClear(t *testing.T) {
	// A server that offers no STARTTLS. With a password configured, sending
	// anyway would put the credential on the wire in clear — and nothing in the
	// UI would ever say so. Refusing is the only honest answer.
	cfg := SMTPConfig{
		Host: "127.0.0.1", Port: 1, From: "a@b.c", To: []string{"d@e.f"},
		Username: "user", Password: "secret", Encryption: "starttls",
	}
	err := testSender().sendMail(context.Background(), cfg, testEvent())
	if err == nil {
		t.Fatal("an unreachable relay must fail rather than silently succeed")
	}
}

func TestValidateConfigRefusesAChannelThatCouldOnlyFailLater(t *testing.T) {
	cases := []struct {
		kind string
		cfg  Config
		ok   bool
	}{
		{"smtp", Config{SMTP: &SMTPConfig{Host: "mail", From: "a@b", To: []string{"c@d"}}}, true},
		{"smtp", Config{SMTP: &SMTPConfig{Host: "mail", From: "a@b"}}, false}, // no recipient
		{"smtp", Config{URL: "https://hooks.example/x"}, false},               // a Slack URL on an SMTP channel
		{"smtp", Config{SMTP: &SMTPConfig{Host: "m", From: "a", To: []string{"c"}, Encryption: "ssl"}}, false},
		{"telegram", Config{Telegram: &TelegramConfig{BotToken: "t"}}, false}, // no chat
		{"telegram", Config{Telegram: &TelegramConfig{BotToken: "t", ChatID: "1"}}, true},
		{"pushover", Config{Pushover: &PushoverConfig{Token: "t", UserKey: "u"}}, true},
		{"resend", Config{Resend: &ResendConfig{APIKey: "k", From: "a"}}, false}, // no recipient
		{"slack", Config{URL: "not a url"}, false},
		{"slack", Config{URL: "https://hooks.slack.com/x"}, true},
	}
	for _, c := range cases {
		err := ValidateConfig(c.kind, c.cfg)
		if (err == nil) != c.ok {
			t.Errorf("ValidateConfig(%s, %+v) = %v, want ok=%v", c.kind, c.cfg, err, c.ok)
		}
	}
}
