package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSeverityParsingFormattingAndSupportedKinds(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  Severity
		text  string
	}{
		{"info", SeverityInfo, "info"},
		{"unknown", SeverityInfo, "info"},
		{"warning", SeverityWarning, "warning"},
		{"critical", SeverityCritical, "critical"},
	} {
		if got := ParseSeverity(tc.input); got != tc.want {
			t.Errorf("ParseSeverity(%q) = %v", tc.input, got)
		}
		if got := tc.want.String(); got != tc.text {
			t.Errorf("%v.String() = %q", tc.want, got)
		}
	}
	for _, kind := range []string{"webhook", "slack", "discord", "smtp", "resend", "telegram", "pushover"} {
		if !Supported(kind) {
			t.Errorf("%q should be supported", kind)
		}
	}
	if Supported("pager") {
		t.Fatal("unknown provider reported as supported")
	}
}

func TestEventTextVariants(t *testing.T) {
	if got := (Event{Type: "deployment.cancelled.v1", Severity: "warning"}).Text(); !strings.HasPrefix(got, "🟠") {
		t.Fatalf("warning text = %q", got)
	}
	if got := (Event{Type: "deployment.succeeded.v1", Severity: "info"}).Text(); !strings.HasPrefix(got, "🟢") {
		t.Fatalf("info text = %q", got)
	}
	digest := Event{
		Type: "notification.digest.v1", Payload: map[string]any{
			"total": 3, "since": "yesterday",
		},
	}
	if got := digest.Text(); !strings.Contains(got, "digest — 3 events since yesterday") {
		t.Fatalf("digest text = %q", got)
	}
}

func TestValidateEveryChannelConfig(t *testing.T) {
	valid := map[string]Config{
		"webhook": {URL: "http://hooks.example.test/x"},
		"slack":   {URL: "https://hooks.example.test/x"},
		"discord": {URL: "https://hooks.example.test/x"},
		"smtp": {SMTP: &SMTPConfig{
			Host: "smtp.example.test", From: "from@example.test", To: []string{"to@example.test"},
		}},
		"resend": {Resend: &ResendConfig{
			APIKey: "secret", From: "from@example.test", To: []string{"to@example.test"},
		}},
		"telegram": {Telegram: &TelegramConfig{BotToken: "secret", ChatID: "12"}},
		"pushover": {Pushover: &PushoverConfig{Token: "secret", UserKey: "user"}},
	}
	for kind, cfg := range valid {
		if err := ValidateConfig(kind, cfg); err != nil {
			t.Errorf("%s valid config: %v", kind, err)
		}
	}
	invalid := []struct {
		kind string
		cfg  Config
	}{
		{"webhook", Config{URL: "ftp://example.test/x"}},
		{"smtp", Config{}},
		{"smtp", Config{SMTP: &SMTPConfig{
			Host: "x", From: "x", To: []string{"x"}, Encryption: "magic",
		}}},
		{"resend", Config{}},
		{"telegram", Config{}},
		{"pushover", Config{}},
		{"pager", Config{}},
	}
	for _, tc := range invalid {
		if err := ValidateConfig(tc.kind, tc.cfg); err == nil {
			t.Errorf("%s invalid config was accepted", tc.kind)
		}
	}
}

func TestSenderHTTPProviders(t *testing.T) {
	type request struct {
		path   string
		header http.Header
		body   map[string]any
	}
	requests := make(chan request, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		requests <- request{path: r.URL.Path, header: r.Header.Clone(), body: body}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	oldResend, oldPushover, oldTelegram := resendEndpoint, pushoverEndpoint, telegramBase
	resendEndpoint = server.URL + "/resend"
	pushoverEndpoint = server.URL + "/pushover"
	telegramBase = server.URL
	defer func() {
		resendEndpoint, pushoverEndpoint, telegramBase = oldResend, oldPushover, oldTelegram
	}()

	event := Event{Type: "deployment.failed.v1", Severity: "critical"}
	cases := []struct {
		kind string
		cfg  Config
		path string
	}{
		{"webhook", Config{URL: server.URL + "/webhook"}, "/webhook"},
		{"slack", Config{URL: server.URL + "/slack"}, "/slack"},
		{"discord", Config{URL: server.URL + "/discord"}, "/discord"},
		{"resend", Config{Resend: &ResendConfig{
			APIKey: "api-secret", From: "from@example.test", To: []string{"to@example.test"},
		}}, "/resend"},
		{"telegram", Config{Telegram: &TelegramConfig{
			BotToken: "bot-secret", ChatID: "chat", TopicID: "topic",
		}}, "/botbot-secret/sendMessage"},
		{"pushover", Config{Pushover: &PushoverConfig{
			Token: "app-secret", UserKey: "user-secret",
		}}, "/pushover"},
	}
	sender := testSender()
	if sender.HTTP.Timeout != 10*time.Second {
		t.Fatalf("HTTP timeout = %s", sender.HTTP.Timeout)
	}
	for _, tc := range cases {
		if err := sender.Send(context.Background(), tc.kind, tc.cfg, event); err != nil {
			t.Fatalf("%s: %v", tc.kind, err)
		}
		got := <-requests
		if got.path != tc.path || got.header.Get("Content-Type") != "application/json" {
			t.Errorf("%s request = %#v", tc.kind, got)
		}
		switch tc.kind {
		case "slack":
			if got.body["text"] == nil || got.body["event"] == nil {
				t.Errorf("slack payload = %#v", got.body)
			}
		case "discord":
			if got.body["content"] == nil {
				t.Errorf("discord payload = %#v", got.body)
			}
		case "resend":
			if got.header.Get("Authorization") != "Bearer api-secret" {
				t.Errorf("resend authorization = %q", got.header.Get("Authorization"))
			}
		case "telegram":
			if got.body["message_thread_id"] != "topic" {
				t.Errorf("telegram payload = %#v", got.body)
			}
		}
	}
}

func TestSenderErrors(t *testing.T) {
	if err := testSender().Send(context.Background(), "smtp", Config{}, Event{}); err == nil {
		t.Fatal("invalid provider config should fail before it can panic")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, strings.Repeat("invalid token ", 40))
	}))
	defer server.Close()
	err := testSender().Send(context.Background(), "webhook", Config{URL: server.URL}, Event{})
	if err == nil || !strings.Contains(err.Error(), "answered 401") || len(err.Error()) > 320 {
		t.Fatalf("HTTP error = %v", err)
	}

	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("network down")
	})}
	err = (&Sender{HTTP: client}).Send(
		context.Background(), "webhook", Config{URL: "https://example.test"}, Event{},
	)
	if err == nil || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("transport error = %v", err)
	}

	old := resendEndpoint
	resendEndpoint = "://bad target"
	defer func() { resendEndpoint = old }()
	err = testSender().Send(context.Background(), "resend", Config{Resend: &ResendConfig{
		APIKey: "x", From: "x@example.test", To: []string{"y@example.test"},
	}}, Event{})
	if err == nil {
		t.Fatal("malformed provider endpoint was accepted")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMailRendering(t *testing.T) {
	event := Event{
		Type: "deployment.failed.v1", Severity: "critical", Resource: "resource-1",
	}
	body := mailBody(event)
	if !strings.Contains(body, "resource: resource-1") ||
		!strings.Contains(body, "severity: critical") {
		t.Fatalf("mail body = %q", body)
	}
	message := string(buildMessage("from@example.test", []string{"a@example.test", "b@example.test"}, event))
	for _, want := range []string{
		"From: from@example.test\r\n",
		"To: a@example.test, b@example.test\r\n",
		"Subject: [akerdock/critical]",
		"Content-Type: text/plain; charset=utf-8",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("message missing %q: %q", want, message)
		}
	}
}

func TestSendMailPlainRelay(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)
		write := func(line string) error {
			if _, err := writer.WriteString(line + "\r\n"); err != nil {
				return err
			}
			return writer.Flush()
		}
		if err := write("220 test ESMTP"); err != nil {
			done <- err
			return
		}
		inData := false
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				done <- err
				return
			}
			line = strings.TrimRight(line, "\r\n")
			if inData {
				if line == "." {
					inData = false
					if err := write("250 queued"); err != nil {
						done <- err
						return
					}
				}
				continue
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				if err := write("250-test"); err != nil {
					done <- err
					return
				}
				if err := write("250 OK"); err != nil {
					done <- err
					return
				}
			case strings.HasPrefix(line, "MAIL FROM:"), strings.HasPrefix(line, "RCPT TO:"):
				if err := write("250 OK"); err != nil {
					done <- err
					return
				}
			case line == "DATA":
				inData = true
				if err := write("354 go ahead"); err != nil {
					done <- err
					return
				}
			case line == "QUIT":
				_ = write("221 bye")
				done <- nil
				return
			default:
				done <- errors.New("unexpected SMTP command: " + line)
				return
			}
		}
	}()

	host, portText, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portText)
	err = testSender().Send(context.Background(), "smtp", Config{SMTP: &SMTPConfig{
		Host: host, Port: port, From: "from@example.test", To: []string{"to@example.test"},
		Encryption: "none",
	}}, Event{Type: "deployment.succeeded.v1"})
	if err != nil {
		t.Fatalf("send mail: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("SMTP server: %v", err)
	}
}

func TestSendMailConnectionAndDowngradeErrors(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	_ = listener.Close()
	host, portText, _ := net.SplitHostPort(addr)
	port, _ := strconv.Atoi(portText)
	err = testSender().Send(context.Background(), "smtp", Config{SMTP: &SMTPConfig{
		Host: host, Port: port, From: "from@example.test", To: []string{"to@example.test"},
		Encryption: "none",
	}}, Event{})
	if err == nil || !strings.Contains(err.Error(), "cannot reach") {
		t.Fatalf("connection error = %v", err)
	}

	listener, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		writer := bufio.NewWriter(conn)
		_, _ = writer.WriteString("220 test ESMTP\r\n")
		_ = writer.Flush()
		_, _ = reader.ReadString('\n')
		_, _ = writer.WriteString("250 OK\r\n")
		_ = writer.Flush()
	}()
	host, portText, _ = net.SplitHostPort(listener.Addr().String())
	port, _ = strconv.Atoi(portText)
	err = testSender().Send(context.Background(), "smtp", Config{SMTP: &SMTPConfig{
		Host: host, Port: port, Username: "user", Password: "secret",
		From: "from@example.test", To: []string{"to@example.test"}, Encryption: "starttls",
	}}, Event{})
	if err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Fatalf("downgrade protection error = %v", err)
	}
}
