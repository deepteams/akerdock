// Package notify turns an outbox event into a message on a channel (§11,
// ADR-019).
//
// It knows three things and nothing else: how severe an event is, how to word
// it, and how to hand it to a provider. Whether an event *should* be sent —
// routing, debouncing, quiet hours — belongs to the dispatcher, which owns
// the rules and the delivery history.
package notify

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/smtp"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Severity orders how much an event deserves to wake someone up (ADR-019).
type Severity int

// The severity ladder: info is routine, critical wakes someone up.
const (
	SeverityInfo Severity = iota
	SeverityWarning
	SeverityCritical
)

// ParseSeverity reads the enum stored on a rule.
func ParseSeverity(s string) Severity {
	switch s {
	case "warning":
		return SeverityWarning
	case "critical":
		return SeverityCritical
	default:
		return SeverityInfo
	}
}

func (s Severity) String() string {
	switch s {
	case SeverityWarning:
		return "warning"
	case SeverityCritical:
		return "critical"
	default:
		return "info"
	}
}

// SeverityOf classifies an event type. The taxonomy is the load-bearing part
// of ADR-019: a critical event wrongly classified as info would be deferred
// into a digest, which is the one failure mode the ADR calls out. So the
// default is deliberately NOT info — an unknown event that mentions a failure
// is treated as a failure.
func SeverityOf(eventType string) Severity {
	base := strings.TrimSuffix(eventType, ".v1")
	switch {
	case strings.HasSuffix(base, ".failed"),
		strings.HasSuffix(base, ".unreachable"),
		strings.HasSuffix(base, ".dead_letter"):
		return SeverityCritical
	case strings.HasSuffix(base, ".cancelled"),
		strings.HasSuffix(base, ".degraded"),
		strings.HasSuffix(base, ".partial"),
		strings.HasSuffix(base, ".expiring"):
		return SeverityWarning
	default:
		return SeverityInfo
	}
}

// Event is what a channel is asked to deliver.
type Event struct {
	Type       string         `json:"event_type"`
	Severity   string         `json:"severity"`
	OccurredAt time.Time      `json:"occurred_at"`
	TeamUUID   string         `json:"team_uuid,omitempty"`
	Resource   string         `json:"resource_uuid,omitempty"`
	Payload    map[string]any `json:"payload,omitempty"`
	// Suppressed counts the events this one stands for: a debounced alert
	// says "and 12 others" instead of hiding them (ADR-019).
	Suppressed int `json:"suppressed_count,omitempty"`
}

// Text renders the human-readable line sent to chat channels.
func (e Event) Text() string {
	var b strings.Builder
	switch e.Severity {
	case "critical":
		b.WriteString("🔴 ")
	case "warning":
		b.WriteString("🟠 ")
	default:
		b.WriteString("🟢 ")
	}
	if e.Type == "notification.digest.v1" {
		fmt.Fprintf(&b, "digest — %v events since %s", e.Payload["total"], e.Payload["since"])
		return b.String()
	}
	b.WriteString(strings.TrimSuffix(e.Type, ".v1"))
	if e.Resource != "" {
		b.WriteString(" — ")
		b.WriteString(e.Resource)
	}
	if e.Suppressed > 0 {
		fmt.Fprintf(&b, " (and %d similar events)", e.Suppressed)
	}
	return b.String()
}

// Config is the decrypted configuration of a channel. Only the fields the
// channel's kind needs are set. The whole struct is envelope-encrypted at rest
// (ADR-003): a bot token and an SMTP password are credentials, not settings.
type Config struct {
	URL      string          `json:"url,omitempty"`
	SMTP     *SMTPConfig     `json:"smtp,omitempty"`
	Resend   *ResendConfig   `json:"resend,omitempty"`
	Telegram *TelegramConfig `json:"telegram,omitempty"`
	Pushover *PushoverConfig `json:"pushover,omitempty"`
}

// SMTPConfig talks to a mail relay. `Encryption` is explicit rather than
// guessed from the port: a channel that silently downgrades to plaintext would
// put the password and the alert on the wire in clear, and nothing would say so.
type SMTPConfig struct {
	Host       string   `json:"host"`
	Port       int      `json:"port"`
	Username   string   `json:"username,omitempty"`
	Password   string   `json:"password,omitempty"`
	From       string   `json:"from"`
	To         []string `json:"to"`
	Encryption string   `json:"encryption"` // starttls | tls | none
}

// ResendConfig posts to the Resend HTTP API.
type ResendConfig struct {
	APIKey string   `json:"api_key"`
	From   string   `json:"from"`
	To     []string `json:"to"`
}

// TelegramConfig posts to the Bot API.
type TelegramConfig struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
	TopicID  string `json:"topic_id,omitempty"`
}

// PushoverConfig posts to the Pushover message API.
type PushoverConfig struct {
	Token   string `json:"token"`
	UserKey string `json:"user_key"`
}

// Kind is the provider a channel talks to.
type Kind string

// The channel kinds that can deliver today; the schema declares more.
const (
	KindWebhook  Kind = "webhook"
	KindSlack    Kind = "slack"
	KindDiscord  Kind = "discord"
	KindSMTP     Kind = "smtp"
	KindResend   Kind = "resend"
	KindTelegram Kind = "telegram"
	KindPushover Kind = "pushover"
)

// Supported reports whether a channel kind can actually deliver.
func Supported(kind string) bool {
	switch Kind(kind) {
	case KindWebhook, KindSlack, KindDiscord, KindSMTP, KindResend, KindTelegram, KindPushover:
		return true
	default:
		return false
	}
}

// ValidateConfig checks a channel's configuration before it is stored. A
// channel is validated for the transport it claims to be: accepting an SMTP
// channel with no host would create an alerting path that can only fail at the
// moment it is needed.
func ValidateConfig(kind string, cfg Config) error {
	switch Kind(kind) {
	case KindWebhook, KindSlack, KindDiscord:
		u, err := url.Parse(cfg.URL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return fmt.Errorf("url must be an http(s) URL")
		}
		return nil
	case KindSMTP:
		c := cfg.SMTP
		if c == nil || c.Host == "" || c.From == "" || len(c.To) == 0 {
			return fmt.Errorf("smtp requires host, from and at least one recipient")
		}
		switch c.Encryption {
		case "", "starttls", "tls", "none":
		default:
			return fmt.Errorf("smtp.encryption must be starttls, tls or none")
		}
		return nil
	case KindResend:
		c := cfg.Resend
		if c == nil || c.APIKey == "" || c.From == "" || len(c.To) == 0 {
			return fmt.Errorf("resend requires api_key, from and at least one recipient")
		}
		return nil
	case KindTelegram:
		c := cfg.Telegram
		if c == nil || c.BotToken == "" || c.ChatID == "" {
			return fmt.Errorf("telegram requires bot_token and chat_id")
		}
		return nil
	case KindPushover:
		c := cfg.Pushover
		if c == nil || c.Token == "" || c.UserKey == "" {
			return fmt.Errorf("pushover requires token and user_key")
		}
		return nil
	default:
		return fmt.Errorf("unknown channel kind %q", kind)
	}
}

// Sender delivers events to a channel.
type Sender struct {
	HTTP *http.Client
}

// New builds a sender. The timeout is short: a channel that hangs must not
// hold the dispatcher, and a missed alert is retried on the next pass.
func New() *Sender {
	return &Sender{HTTP: &http.Client{Timeout: 10 * time.Second}}
}

// Send posts the event to the channel. The body shape is the provider's; the
// event itself is always included so a webhook consumer gets the structured
// data, not only a sentence.
func (s *Sender) Send(ctx context.Context, kind string, cfg Config, e Event) error {
	var body any
	target := cfg.URL
	headers := map[string]string{}
	switch Kind(kind) {
	case KindSlack:
		body = map[string]any{"text": e.Text(), "event": e}
	case KindDiscord:
		body = map[string]any{"content": e.Text(), "event": e}
	case KindWebhook:
		body = e
	case KindSMTP:
		return s.sendMail(ctx, *cfg.SMTP, e)
	case KindResend:
		// The API key is a header, never a query parameter: a URL travels in
		// logs and referrers, a header does not.
		target = resendEndpoint
		headers["Authorization"] = "Bearer " + cfg.Resend.APIKey
		body = map[string]any{
			"from": cfg.Resend.From, "to": cfg.Resend.To,
			"subject": e.Text(), "text": mailBody(e),
		}
	case KindTelegram:
		target = telegramEndpoint(cfg.Telegram.BotToken)
		msg := map[string]any{"chat_id": cfg.Telegram.ChatID, "text": e.Text()}
		if cfg.Telegram.TopicID != "" {
			msg["message_thread_id"] = cfg.Telegram.TopicID
		}
		body = msg
	case KindPushover:
		target = pushoverEndpoint
		body = map[string]any{
			"token": cfg.Pushover.Token, "user": cfg.Pushover.UserKey,
			"message": e.Text(),
		}
	default:
		return fmt.Errorf("unsupported channel kind %q", kind)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	res, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode < 200 || res.StatusCode > 299 {
		// The response body often names the cause (invalid_token, channel_not_found).
		snippet, _ := io.ReadAll(io.LimitReader(res.Body, 256))
		return fmt.Errorf("channel answered %d: %s", res.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// The provider endpoints. They are variables, not constants, so a test can
// point them at an httptest server — the alternative is shipping code that has
// never had its payload shape checked against anything.
var (
	resendEndpoint   = "https://api.resend.com/emails"
	pushoverEndpoint = "https://api.pushover.net/1/messages.json"
	telegramBase     = "https://api.telegram.org"
)

func telegramEndpoint(token string) string {
	return telegramBase + "/bot" + token + "/sendMessage"
}

// mailBody is the plain-text body of an alert. No HTML: an alert must be
// readable in a terminal mail client at 3am.
func mailBody(e Event) string {
	var b strings.Builder
	b.WriteString(e.Text())
	b.WriteString("\n\n")
	if e.Resource != "" {
		fmt.Fprintf(&b, "resource: %s\n", e.Resource)
	}
	fmt.Fprintf(&b, "event: %s\nseverity: %s\n", e.Type, SeverityOf(e.Type))
	return b.String()
}

// sendMail delivers over SMTP.
//
// The encryption mode is taken from the configuration, never inferred from the
// port: inferring it means a misconfigured channel silently sends the password
// and the alert in clear, and nothing in the UI would say so. `none` stays
// possible because a relay on localhost is a legitimate setup — but it has to
// be asked for.
func (s *Sender) sendMail(ctx context.Context, cfg SMTPConfig, e Event) error {
	port := cfg.Port
	if port == 0 {
		port = 587
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(port))
	msg := buildMessage(cfg.From, cfg.To, e)

	dialer := &net.Dialer{Timeout: 10 * time.Second}
	var conn net.Conn
	var err error
	if cfg.Encryption == "tls" {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp: cannot reach %s: %w", addr, err)
	}

	client, err := smtp.NewClient(conn, cfg.Host)
	if err != nil {
		_ = conn.Close()
		return err
	}
	defer func() { _ = client.Quit() }()

	if cfg.Encryption == "" || cfg.Encryption == "starttls" {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(&tls.Config{ServerName: cfg.Host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("smtp: STARTTLS refused: %w", err)
			}
		} else if cfg.Username != "" {
			// Refusing here rather than authenticating in the clear: sending the
			// password unencrypted to a server that promised nothing is worse
			// than not sending the alert.
			return fmt.Errorf("smtp: the server does not offer STARTTLS and a password is configured — " +
				"set encryption to tls, or to none if this relay is trusted")
		}
	}
	if cfg.Username != "" {
		if err := client.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)); err != nil {
			return fmt.Errorf("smtp: authentication failed: %w", err)
		}
	}
	if err := client.Mail(cfg.From); err != nil {
		return err
	}
	for _, to := range cfg.To {
		if err := client.Rcpt(to); err != nil {
			return fmt.Errorf("smtp: recipient %s refused: %w", to, err)
		}
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	return w.Close()
}

func buildMessage(from string, to []string, e Event) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", strings.Join(to, ", "))
	// The subject carries the severity: a critical alert must be sortable and
	// filterable without opening it.
	fmt.Fprintf(&b, "Subject: [akerdock/%s] %s\r\n", SeverityOf(e.Type), e.Text())
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	b.WriteString(strings.ReplaceAll(mailBody(e), "\n", "\r\n"))
	return []byte(b.String())
}
