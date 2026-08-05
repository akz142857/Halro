package safelog

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`gw_[A-Za-z0-9_-]{16,}`),
	// Heimdall issues these itself: hms_ admin sessions, hmt_ metrics bearers.
	regexp.MustCompile(`hm[st]_[A-Za-z0-9_-]{20,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{20,}`),
	regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`),
	regexp.MustCompile(`(?s)-----BEGIN[ \t]+(?:RSA |EC |OPENSSH )?PRIVATE KEY-----.*`),
}

// sensitiveKeys redact on the attribute name alone. Matching a value against
// secretPatterns only works for formats already listed there, so the name is
// the only thing standing between an unrecognised credential — an Azure key, a
// self-hosted provider's token, whatever is added next — and the log.
var sensitiveKeys = []string{
	"authorization", "cookie", "secret", "token",
	"password", "passphrase", "credential", "key",
}

func New(handler slog.Handler) *slog.Logger {
	return slog.New(&redactingHandler{next: handler})
}

type redactingHandler struct {
	next slog.Handler
}

func (h *redactingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h *redactingHandler) Handle(ctx context.Context, record slog.Record) error {
	safe := slog.NewRecord(record.Time, record.Level, Redact(record.Message), record.PC)
	record.Attrs(func(attribute slog.Attr) bool {
		safe.AddAttrs(redactAttr(attribute))
		return true
	})
	return h.next.Handle(ctx, safe)
}

func (h *redactingHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	safe := make([]slog.Attr, 0, len(attributes))
	for _, attribute := range attributes {
		safe = append(safe, redactAttr(attribute))
	}
	return &redactingHandler{next: h.next.WithAttrs(safe)}
}

func (h *redactingHandler) WithGroup(name string) slog.Handler {
	return &redactingHandler{next: h.next.WithGroup(name)}
}

func redactAttr(attribute slog.Attr) slog.Attr {
	// A LogValuer picks its own representation, and it picks it downstream of
	// here — leaving it unresolved hands the next handler a value this function
	// never looked at.
	attribute.Value = attribute.Value.Resolve()
	key := strings.ToLower(attribute.Key)
	for _, sensitive := range sensitiveKeys {
		if strings.Contains(key, sensitive) {
			return slog.String(attribute.Key, "[REDACTED]")
		}
	}
	switch attribute.Value.Kind() {
	case slog.KindString:
		return slog.String(attribute.Key, Redact(attribute.Value.String()))
	case slog.KindGroup:
		members := attribute.Value.Group()
		safe := make([]slog.Attr, 0, len(members))
		for _, member := range members {
			safe = append(safe, redactAttr(member))
		}
		return slog.Attr{Key: attribute.Key, Value: slog.GroupValue(safe...)}
	case slog.KindAny:
		return slog.String(attribute.Key, Redact(render(attribute.Value.Any())))
	}
	// What remains is numeric, boolean, or time-shaped and cannot carry a
	// credential in text form, so it passes through with its type intact.
	return attribute
}

// render flattens a value of unknown shape into text the patterns can scan.
// Passing it on unrendered would leave the next handler to serialise it, and a
// struct field or byte slice holding a credential would go out verbatim.
func render(value any) string {
	switch typed := value.(type) {
	case error:
		return typed.Error()
	case []byte:
		// Printed with %v a byte slice becomes decimal codes, which no pattern
		// can match and which a reader can still decode.
		return string(typed)
	case fmt.Stringer:
		return typed.String()
	default:
		return fmt.Sprint(value)
	}
}

func Redact(value string) string {
	for _, pattern := range secretPatterns {
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	return value
}
