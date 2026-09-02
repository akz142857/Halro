package safelog

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// TestSecretPatternsCannotMatchEmpty guards the premise Redact's match guard
// stands on. Skipping ReplaceAllString when MatchString says no is equivalent
// only for patterns that need at least one character; one that could match the
// empty string would have rewritten every input, and the guard would silently
// stop it from doing so.
//
// This is checked rather than argued in a comment because the failure mode is a
// pattern added later — its author would be reading their own regex, not this
// one's contract.
func TestSecretPatternsCannotMatchEmpty(t *testing.T) {
	for _, pattern := range secretPatterns {
		if pattern.MatchString("") {
			t.Fatalf("pattern %q matches the empty string, which breaks Redact's match guard", pattern)
		}
	}
}

// TestRedactGuardMatchesUnguarded pins the equivalence itself across the shapes
// this suite cares about, so a change to either side has to justify a
// difference rather than produce one quietly.
func TestRedactGuardMatchesUnguarded(t *testing.T) {
	unguarded := func(value string) string {
		for _, pattern := range secretPatterns {
			value = pattern.ReplaceAllString(value, "[REDACTED]")
		}
		return value
	}
	for _, value := range []string{
		"", "request failed", "deployment_01J8ZQ2K3M4N5P6Q7R8S9T0V",
		"Bearer provider-secret", "sk-abcdefghijk", "gw_abcdefghijklmnopqrstuvwxyz0123456789",
		"AIza0123456789abcdefghijklmnopqrst", "ASIA0123456789ABCDEF", "AKIA0123456789ABCDEF",
		"hms_abcdefghijklmnopqrstuvwxyz", "hmt_abcdefghijklmnopqrstuvwxyz",
		"-----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY-----",
		"prefix Bearer abc then sk-defghijk and gw_abcdefghijklmnopqrstuvwxyz0123456789 suffix",
	} {
		if got, want := Redact(value), unguarded(value); got != want {
			t.Fatalf("the match guard changed the result for %q: got %q, want %q", value, got, want)
		}
	}
}

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
