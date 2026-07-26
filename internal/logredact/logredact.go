// Package logredact wraps a slog.Handler to mask the values of sensitive
// attributes before they are written anywhere (§23.4, INV-003, ISO A.8.12).
//
// Redaction lives in a wrapping handler, not in a JSON/text handler's
// ReplaceAttr, because the logs fan out to more than one sink (local stderr AND
// the OTLP exporter): redacting the record once, before the fan-out, covers
// every sink from a single place. A secret written to a log is a second copy of
// that secret, in a stream that leaves the instance.
package logredact

import (
	"context"
	"log/slog"
	"strings"
)

// Placeholder replaces a redacted value.
const Placeholder = "[REDACTED]"

// sensitiveKeys are attribute keys whose VALUE is a credential. Matched
// case-insensitively, exact. Identifier keys like token_uuid / token_id are
// deliberately NOT here — they name a secret, they are not the secret.
var sensitiveKeys = map[string]bool{
	"password": true, "passwd": true, "secret": true, "token": true,
	"authorization": true, "credential": true, "credentials": true,
	"api_key": true, "apikey": true, "private_key": true, "privatekey": true,
	"access_key": true, "secret_key": true, "client_secret": true,
	"bot_token": true, "csrf": true, "cookie": true, "set-cookie": true,
}

// sensitiveSuffixes catch the common "<thing>_token/_secret/_password" naming.
var sensitiveSuffixes = []string{"_token", "_secret", "_password"}

func sensitive(key string) bool {
	k := strings.ToLower(key)
	if sensitiveKeys[k] {
		return true
	}
	for _, s := range sensitiveSuffixes {
		if strings.HasSuffix(k, s) {
			return true
		}
	}
	return false
}

// redact returns the attribute with a sensitive value masked, recursing into
// group values so a secret nested under a group is caught too.
func redact(a slog.Attr) slog.Attr {
	if sensitive(a.Key) {
		return slog.String(a.Key, Placeholder)
	}
	if a.Value.Kind() == slog.KindGroup {
		group := a.Value.Group()
		out := make([]any, 0, len(group))
		for _, ga := range group {
			out = append(out, redact(ga))
		}
		return slog.Group(a.Key, out...)
	}
	return a
}

// Handler redacts sensitive attributes then delegates to the wrapped handler.
type Handler struct{ inner slog.Handler }

// Wrap returns h with redaction applied to every record it handles.
func Wrap(h slog.Handler) slog.Handler { return Handler{inner: h} }

func (h Handler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h Handler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redact(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h Handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(attrs))
	for i, a := range attrs {
		red[i] = redact(a)
	}
	return Handler{inner: h.inner.WithAttrs(red)}
}

func (h Handler) WithGroup(name string) slog.Handler {
	return Handler{inner: h.inner.WithGroup(name)}
}
