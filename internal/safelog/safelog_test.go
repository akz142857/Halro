package safelog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestLoggerRedactsSensitiveKeysAndValues(t *testing.T) {
	var output bytes.Buffer
	logger := New(slog.NewJSONHandler(&output, nil))
	logger.LogAttrs(context.Background(), slog.LevelInfo, "using gw_abcdefghijklmnopqrstuvwxyz0123456789",
		slog.String("authorization", "Bearer provider-secret"),
		slog.String("detail", "sk-abcdefghijk"),
		slog.String("google", "AIza0123456789abcdefghijklmnopqrst"),
		slog.String("aws", "ASIA0123456789ABCDEF"),
		slog.String("pem", "-----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY-----"),
	)
	got := output.String()
	for _, secret := range []string{
		"gw_abcdefghijklmnopqrstuvwxyz0123456789", "provider-secret", "sk-abcdefghijk",
		"AIza0123456789abcdefghijklmnopqrst", "ASIA0123456789ABCDEF", "private-material",
	} {
		if strings.Contains(got, secret) {
			t.Fatalf("log leaked %q: %s", secret, got)
		}
	}
}
