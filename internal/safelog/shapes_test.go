package safelog

import (
	"bytes"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

const canary = "sk-live-canary-must-never-reach-the-log"

// A handler encodes before it writes — JSONHandler base64s a byte slice — so
// searching the output for the canary verbatim would call a trivially
// decodable leak a pass.
func leaked(output string) bool {
	return strings.Contains(output, canary) ||
		strings.Contains(output, base64.StdEncoding.EncodeToString([]byte(canary)))
}

type credentialValuer struct{ secret string }

func (c credentialValuer) LogValue() slog.Value { return slog.StringValue(c.secret) }

type credentialStringer struct{ secret string }

func (c credentialStringer) String() string { return c.secret }

type providerConfig struct {
	Endpoint string
	APIKey   string
}

// Redaction used to inspect only top-level strings and errors, so a credential
// carried inside a group, behind a LogValuer, or in a struct passed straight
// through to the handler that serialises it. The single test that existed fed
// it strings alone and kept passing the whole time, which is what made the gap
// survivable. This drives one canary through every shape slog can carry.
func TestNoAttributeShapeCarriesACredentialToTheHandler(t *testing.T) {
	for _, testCase := range []struct {
		name string
		emit func(*slog.Logger)
	}{
		{"top level string", func(l *slog.Logger) {
			l.Info("call failed", slog.String("detail", canary))
		}},
		{"inside a group", func(l *slog.Logger) {
			l.Info("call failed", slog.Group("upstream", slog.String("detail", canary)))
		}},
		{"inside a nested group", func(l *slog.Logger) {
			l.Info("call failed", slog.Group("upstream",
				slog.Group("request", slog.String("detail", canary))))
		}},
		{"group keyed as sensitive", func(l *slog.Logger) {
			l.Info("call failed", slog.Group("credentials", slog.String("value", canary)))
		}},
		{"struct behind Any", func(l *slog.Logger) {
			l.Info("call failed", slog.Any("config", providerConfig{Endpoint: "https://x", APIKey: canary}))
		}},
		{"byte slice behind Any", func(l *slog.Logger) {
			l.Info("call failed", slog.Any("material", []byte(canary)))
		}},
		{"LogValuer", func(l *slog.Logger) {
			l.Info("call failed", slog.Any("provider", credentialValuer{secret: canary}))
		}},
		{"Stringer", func(l *slog.Logger) {
			l.Info("call failed", slog.Any("provider", credentialStringer{secret: canary}))
		}},
		{"error", func(l *slog.Logger) {
			l.Info("call failed", slog.Any("error", errors.New("dial failed for "+canary)))
		}},
		{"through With", func(l *slog.Logger) {
			l.With(slog.String("detail", canary)).Info("call failed")
		}},
		{"through With and a group", func(l *slog.Logger) {
			l.With(slog.Group("upstream", slog.String("detail", canary))).Info("call failed")
		}},
		{"through WithGroup", func(l *slog.Logger) {
			l.WithGroup("upstream").Info("call failed", slog.String("detail", canary))
		}},
		{"in the message", func(l *slog.Logger) {
			l.Info("call failed for " + canary)
		}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var output bytes.Buffer
			testCase.emit(New(slog.NewJSONHandler(&output, nil)))
			if leaked(output.String()) {
				t.Fatalf("credential reached the handler: %s", output.String())
			}
		})
	}
}

// Pattern matching only covers formats already listed, so an unrecognised
// credential is caught by its attribute name or not at all. These names carry
// values no pattern would match.
func TestUnrecognisedCredentialFormatsAreCaughtByAttributeName(t *testing.T) {
	for _, name := range []string{
		"api_key", "apikey", "private_key", "access_key",
		"passphrase", "client_secret", "session_token", "x_credential",
	} {
		t.Run(name, func(t *testing.T) {
			var output bytes.Buffer
			opaque := "3f9a1c77e0b24d5e8a6f1029bc4d7e30"
			New(slog.NewJSONHandler(&output, nil)).
				Info("configured", slog.String(name, opaque))
			if strings.Contains(output.String(), opaque) {
				t.Fatalf("attribute %q leaked an opaque credential: %s", name, output.String())
			}
		})
	}
}

// Heimdall mints these two itself, so nothing else will recognise them.
func TestOwnIssuedTokenFormatsAreRedacted(t *testing.T) {
	for _, token := range []string{
		"hms_dGhpcy1pcy1hLTMyLWJ5dGUtc2Vzc2lvbi10b2tlbg",
		"hmt_dGhpcy1pcy1hLTMyLWJ5dGUtbWV0cmljcy10b2tlbg",
	} {
		if redacted := Redact("presented " + token); strings.Contains(redacted, token) {
			t.Fatalf("Redact left %q intact: %s", token, redacted)
		}
	}
}

// Over-redaction has a cost too: an operator reading these logs needs the
// identifiers that make an incident traceable.
func TestOrdinaryDiagnosticAttributesSurvive(t *testing.T) {
	var output bytes.Buffer
	New(slog.NewJSONHandler(&output, nil)).Info("probe finished",
		slog.String("target_id", "heimdall-primary"),
		slog.String("component", "deadman"),
		slog.Int("status", 503),
		slog.Bool("degraded", true),
	)
	got := output.String()
	for _, expected := range []string{"heimdall-primary", "deadman", "503", "true"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("redaction swallowed the diagnostic %q: %s", expected, got)
		}
	}
}
