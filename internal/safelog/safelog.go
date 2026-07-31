package safelog

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
)

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)Bearer\s+[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`gw_[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{20,}`),
	regexp.MustCompile(`(?:AKIA|ASIA)[0-9A-Z]{16}`),
	regexp.MustCompile(`(?s)-----BEGIN[ \t]+(?:RSA |EC |OPENSSH )?PRIVATE KEY-----.*`),
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
	key := strings.ToLower(attribute.Key)
	for _, sensitive := range []string{"authorization", "cookie", "secret", "token", "password", "credential"} {
		if strings.Contains(key, sensitive) {
			return slog.String(attribute.Key, "[REDACTED]")
		}
	}
	switch attribute.Value.Kind() {
	case slog.KindString:
		return slog.String(attribute.Key, Redact(attribute.Value.String()))
	case slog.KindAny:
		if err, ok := attribute.Value.Any().(error); ok {
			return slog.String(attribute.Key, Redact(err.Error()))
		}
	}
	return attribute
}

func Redact(value string) string {
	for _, pattern := range secretPatterns {
		value = pattern.ReplaceAllString(value, "[REDACTED]")
	}
	return value
}
