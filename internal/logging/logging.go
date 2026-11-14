// Package logging builds the process logger: log/slog, text or JSON, with a
// handler that redacts secrets before any record is written.
//
// The code that logs is written not to pass secrets to the logger at all.
// The redacting handler is a second line: it replaces
//   - the value of any attribute whose key names a secret (token, secret,
//     password, authorization, cookie, session, csrf, private key, env);
//   - any value of type config.Secret;
//   - admin tokens (hm_...), join tokens (hivemind-join-v1....), bearer
//     credentials and PEM private key blocks found inside any string, error
//     or formatted value, the message included.
//
// It cannot recognise a session ID, a CSRF token or an env value by its
// shape, so those rely on the key rule and on the callers.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"regexp"
	"strings"

	"github.com/ao/hivemind/internal/config"
)

// Mask replaces a redacted value.
const Mask = "[redacted]"

// New returns a logger that writes to w at level ("debug", "info", "warn" or
// "error") in format ("text" or "json"), through the redacting handler.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		return nil, fmt.Errorf("log level %q: %w", level, err)
	}
	opts := &slog.HandlerOptions{Level: lv}
	var h slog.Handler
	switch format {
	case "text", "":
		h = slog.NewTextHandler(w, opts)
	case "json":
		h = slog.NewJSONHandler(w, opts)
	default:
		return nil, fmt.Errorf("log format %q: must be text or json", format)
	}
	return slog.New(Redact(h)), nil
}

// Redact wraps h so that secrets are removed from every record and from
// attributes added with WithAttrs.
func Redact(h slog.Handler) slog.Handler { return &redactor{next: h} }

type redactor struct {
	next slog.Handler
	// secretGroup is true inside a WithGroup whose name names a secret:
	// every attribute added there is masked.
	secretGroup bool
}

func (r *redactor) Enabled(ctx context.Context, l slog.Level) bool { return r.next.Enabled(ctx, l) }

func (r *redactor) Handle(ctx context.Context, rec slog.Record) error {
	out := slog.NewRecord(rec.Time, rec.Level, Scrub(rec.Message), rec.PC)
	rec.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(r.attr(a, r.secretGroup))
		return true
	})
	return r.next.Handle(ctx, out)
}

func (r *redactor) WithAttrs(as []slog.Attr) slog.Handler {
	clean := make([]slog.Attr, len(as))
	for i, a := range as {
		clean[i] = r.attr(a, r.secretGroup)
	}
	return &redactor{next: r.next.WithAttrs(clean), secretGroup: r.secretGroup}
}

func (r *redactor) WithGroup(name string) slog.Handler {
	return &redactor{next: r.next.WithGroup(name), secretGroup: r.secretGroup || SecretKey(name)}
}

// attr returns a with secrets masked. masked is true inside a group whose
// name names a secret.
func (r *redactor) attr(a slog.Attr, masked bool) slog.Attr {
	v := a.Value.Resolve()
	if masked || SecretKey(a.Key) {
		if v.Kind() == slog.KindGroup {
			return slog.Attr{Key: a.Key, Value: r.group(v.Group(), true)}
		}
		return slog.String(a.Key, Mask)
	}
	switch v.Kind() {
	case slog.KindGroup:
		return slog.Attr{Key: a.Key, Value: r.group(v.Group(), false)}
	case slog.KindString:
		return slog.String(a.Key, Scrub(v.String()))
	case slog.KindAny:
		return slog.Attr{Key: a.Key, Value: scrubAny(v)}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

func (r *redactor) group(as []slog.Attr, masked bool) slog.Value {
	out := make([]slog.Attr, len(as))
	for i, a := range as {
		out[i] = r.attr(a, masked)
	}
	return slog.GroupValue(out...)
}

// scrubAny masks a config.Secret, and replaces any other value whose
// formatted text holds a secret pattern with the scrubbed text. Values with
// no secret in them pass through unchanged, so JSON output keeps their shape.
func scrubAny(v slog.Value) slog.Value {
	switch x := v.Any().(type) {
	case config.Secret, *config.Secret:
		return slog.StringValue(Mask)
	case error:
		s := x.Error()
		if c := Scrub(s); c != s {
			return slog.StringValue(c)
		}
		return v
	case []byte:
		s := string(x)
		if c := Scrub(s); c != s {
			return slog.StringValue(c)
		}
		return v
	}
	s := fmt.Sprintf("%+v", v.Any())
	if c := Scrub(s); c != s {
		return slog.StringValue(c)
	}
	return v
}

// secretKeyParts are matched against attribute keys with case, dashes and
// underscores ignored.
var secretKeyParts = []string{
	"token", "secret", "password", "passwd", "authorization", "cookie",
	"session", "csrf", "privatekey", "apikey", "credential", "env",
}

// SecretKey reports whether an attribute key names a secret.
func SecretKey(key string) bool {
	k := strings.ToLower(strings.NewReplacer("-", "", "_", "", ".", "").Replace(key))
	if k == "key" || strings.HasSuffix(k, "tlskey") {
		return true
	}
	for _, p := range secretKeyParts {
		if strings.Contains(k, p) {
			return true
		}
	}
	return false
}

var secretPatterns = []*regexp.Regexp{
	// A PEM private key block, or the start of one cut off.
	regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?s:.*?)(-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`),
	// Admin tokens: hm_ and 43 base64url characters.
	regexp.MustCompile(`hm_[A-Za-z0-9_-]{43}`),
	// Join tokens: hivemind-join-v1.<64 hex>.<43 base64url>, and any prefix
	// of one after the version marker.
	regexp.MustCompile(`hivemind-join-v1\.[A-Za-z0-9._-]*`),
	// Any bearer credential.
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`),
}

// Scrub replaces every secret pattern in s with Mask.
func Scrub(s string) string {
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, Mask)
	}
	return s
}
